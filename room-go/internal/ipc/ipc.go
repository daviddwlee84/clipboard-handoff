// Package ipc defines the local client<->daemon contract (PROTOCOL §4):
// length-prefixed CBOR requests/responses over a Unix domain socket, plus a
// small client helper that dials the daemon (auto-spawning it if absent).
package ipc

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

// IPCVersion is bumped on any breaking IPC change (PROTOCOL §5). A client and
// daemon of mismatched versions must fail loudly.
const IPCVersion = 1

// Request is a client->daemon message. Op selects the operation.
type Request struct {
	IPCVersion int    `cbor:"ipc_version"`
	Op         string `cbor:"op"`

	// send
	Bytes []byte `cbor:"bytes,omitempty"` // raw stdin; daemon sniffs
	Force string `cbor:"force,omitempty"` // "", "text", or "image" (--text/--image)

	// recv
	Kind   string `cbor:"kind,omitempty"`   // any | text | image
	Follow bool   `cbor:"follow,omitempty"` // stream until closed

	// config
	Key   string `cbor:"key,omitempty"`
	Value string `cbor:"value,omitempty"`

	// join
	Target string `cbor:"target,omitempty"` // user@host:port
}

// Operation names.
const (
	OpSend      = "send"
	OpRecv      = "recv"
	OpPaste     = "paste"
	OpSubscribe = "subscribe"
	OpStatus    = "status"
	OpConfigSet = "config_set"
	OpConfigGet = "config_get"
	OpJoin      = "join"
	OpPing      = "ping"
)

// Item is a received clipboard item surfaced to a client (PROTOCOL §4 Event).
type Item struct {
	Envelope  wire.Envelope `cbor:"envelope"`
	LocalPath string        `cbor:"local_path,omitempty"` // set for images (materialized PNG)
}

// Status is the daemon state snapshot (SPEC §2 `status`).
type Status struct {
	Fingerprint string `cbor:"fingerprint"`
	DeviceName  string `cbor:"device_name"`
	Room        string `cbor:"room"`
	Server      string `cbor:"server"`
	Connected   bool   `cbor:"connected"`
	AutoCopy    string `cbor:"auto_copy"`
	Peers       int    `cbor:"peers"`
	Buffer      int    `cbor:"buffer"`
}

// Response is a daemon->client message. Multiple responses may stream on one
// connection (subscribe / recv --follow).
type Response struct {
	IPCVersion int     `cbor:"ipc_version"`
	Kind       string  `cbor:"kind"` // ok | err | item | status | value
	Code       int     `cbor:"code,omitempty"`
	Message    string  `cbor:"message,omitempty"`
	Item       *Item   `cbor:"item,omitempty"`
	Status     *Status `cbor:"status,omitempty"`
	Value      string  `cbor:"value,omitempty"`
}

// Response kinds.
const (
	RespOK     = "ok"
	RespErr    = "err"
	RespItem   = "item"
	RespStatus = "status"
	RespValue  = "value"
)

// WriteReq / ReadReq / WriteResp / ReadResp frame CBOR with wire's length
// prefix so IPC and the SSH channel share one framing.
func WriteReq(w interface{ Write([]byte) (int, error) }, r *Request) error {
	b, err := cbor.Marshal(r)
	if err != nil {
		return err
	}
	return wire.WriteFrame(w, b)
}

func ReadReq(rd interface{ Read([]byte) (int, error) }) (*Request, error) {
	b, err := wire.ReadFrame(rd)
	if err != nil {
		return nil, err
	}
	var r Request
	if err := cbor.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func WriteResp(w interface{ Write([]byte) (int, error) }, r *Response) error {
	b, err := cbor.Marshal(r)
	if err != nil {
		return err
	}
	return wire.WriteFrame(w, b)
}

func ReadResp(rd interface{ Read([]byte) (int, error) }) (*Response, error) {
	b, err := wire.ReadFrame(rd)
	if err != nil {
		return nil, err
	}
	var r Response
	if err := cbor.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SocketPath resolves the IPC socket path. An explicit --socket wins; else
// $XDG_RUNTIME_DIR/room/daemon.sock (fallback temp dir), per PROTOCOL §4.
func SocketPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "room")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.sock"), nil
}

// Dial connects to the daemon at socketPath. If no daemon is listening and
// autoSpawn is set, it spawns `room daemon` (detached) with the given global
// flags and retries with a short backoff (PROTOCOL §4).
func Dial(socketPath string, autoSpawn bool, spawnArgs []string) (net.Conn, error) {
	conn, err := net.Dial("unix", socketPath)
	if err == nil {
		return conn, nil
	}
	if !autoSpawn {
		return nil, err
	}

	if serr := spawnDaemon(spawnArgs); serr != nil {
		return nil, fmt.Errorf("spawn daemon: %w", serr)
	}
	// Wait for the socket with a short backoff.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", socketPath)
		if err == nil {
			return conn, nil
		}
		time.Sleep(75 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not come up on %s: %w", socketPath, err)
}

// spawnDaemon launches `room daemon` detached, inheriting the global flags.
func spawnDaemon(spawnArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := append(append([]string{}, spawnArgs...), "daemon")
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	// Detach: new session so it survives the client exiting.
	cmd.SysProcAttr = detachAttr()
	// Log to a file in the runtime dir so failures are diagnosable.
	if logf, lerr := os.CreateTemp("", "room-daemon-*.log"); lerr == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	return cmd.Start()
}
