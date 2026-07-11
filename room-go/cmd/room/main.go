// Command room is the Phase 0 room-go binary: a central SSH room server plus a
// native client daemon and thin client subcommands, all sharing the CLI
// surface in docs/SPEC.md. See README.md for the exact server/join/daemon
// commands used by the bake-off harness.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/log"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/broker"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/config"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/daemon"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/server"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/tui"
	gossh "golang.org/x/crypto/ssh"
)

// Globals are the shared flags (SPEC §2) that may precede the subcommand.
type Globals struct {
	ConfigDir string
	Socket    string
	Room      string
	JSON      bool
	Quiet     bool
	Verbose   bool
}

func main() {
	g, rest := parseGlobals(os.Args[1:])
	if len(rest) == 0 {
		usage()
		os.Exit(2)
	}
	sub, args := rest[0], rest[1:]

	if g.Verbose {
		log.SetLevel(log.DebugLevel)
	} else if g.Quiet {
		log.SetLevel(log.ErrorLevel)
	}

	os.Exit(dispatch(g, sub, args))
}

func dispatch(g Globals, sub string, args []string) int {
	switch sub {
	case "server":
		return cmdServer(g, args)
	case "daemon":
		return cmdDaemon(g, args)
	case "join":
		return cmdJoin(g, args)
	case "send":
		return cmdSend(g, args)
	case "recv":
		return cmdRecv(g, args)
	case "paste":
		return cmdPaste(g, args)
	case "tui":
		return cmdTui(g, args)
	case "status":
		return cmdStatus(g, args)
	case "peers":
		return cmdStatus(g, args) // Phase 0: peers reuses status output
	case "config":
		return cmdConfig(g, args)
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "room: unknown command %q\n", sub)
		usage()
		return 2
	}
}

// parseGlobals pulls the recognized global flags out of args wherever they
// appear (before or after the subcommand), returning the remainder. Only known
// global flag names are consumed, so subcommand-specific flags are untouched.
func parseGlobals(args []string) (Globals, []string) {
	var g Globals
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inlineVal, hasInline := a, "", false
		if eq := strings.Index(a, "="); strings.HasPrefix(a, "-") && eq >= 0 {
			name, inlineVal, hasInline = a[:eq], a[eq+1:], true
		}
		next := func() string {
			if hasInline {
				return inlineVal
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch name {
		case "--config-dir":
			g.ConfigDir = next()
		case "--socket":
			g.Socket = next()
		case "--room":
			g.Room = next()
		case "--json":
			g.JSON = true
		case "-q", "--quiet":
			g.Quiet = true
		case "-v", "--verbose":
			g.Verbose = true
		default:
			rest = append(rest, a)
		}
	}
	return g, rest
}

// spawnArgs reconstructs the global flags so an auto-spawned daemon inherits
// this client's config dir / socket / room.
func (g Globals) spawnArgs() []string {
	var a []string
	if g.ConfigDir != "" {
		a = append(a, "--config-dir", g.ConfigDir)
	}
	if g.Socket != "" {
		a = append(a, "--socket", g.Socket)
	}
	if g.Room != "" {
		a = append(a, "--room", g.Room)
	}
	return a
}

func (g Globals) socketPath() (string, error) { return ipc.SocketPath(g.Socket) }

// ---- server ----------------------------------------------------------------

func cmdServer(g Globals, args []string) int {
	fs := newFlagSet("server")
	addr := fs.string("addr", ":2222", "SSH listen address")
	hostKey := fs.string("host-key", "", "SSH host key path (auto-generated if missing)")
	authKeys := fs.string("authorized-keys", "", "optional authorized_keys file; empty = trust any key (Phase 0)")
	if code := fs.parse(args); code != 0 {
		return code
	}

	hk := *hostKey
	if hk == "" {
		dir, err := resolveConfigDir(g)
		if err != nil {
			return fail(1, err)
		}
		hk = filepath.Join(dir, "host_ed25519")
	}
	if err := os.MkdirAll(filepath.Dir(hk), 0o700); err != nil {
		return fail(1, err)
	}

	allowed, err := loadAuthorizedKeys(*authKeys)
	if err != nil {
		return fail(1, err)
	}

	b := broker.New()
	srv, err := server.New(server.Config{Addr: *addr, HostKey: hk, AuthorizedKeys: allowed}, b)
	if err != nil {
		return fail(1, fmt.Errorf("server.New: %w", err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("room server listening", "addr", *addr, "host-key", hk, "trust", trustLabel(allowed))
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, io.EOF) {
			return fail(1, err)
		}
	case <-ctx.Done():
		log.Info("shutting down server")
		_ = srv.Close()
	}
	return 0
}

func loadAuthorizedKeys(path string) (map[string]bool, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	rest := b
	for len(rest) > 0 {
		key, _, _, remaining, perr := gossh.ParseAuthorizedKey(rest)
		if perr != nil {
			break
		}
		out[gossh.FingerprintSHA256(key)] = true
		rest = remaining
	}
	return out, nil
}

func trustLabel(allowed map[string]bool) string {
	if len(allowed) == 0 {
		return "any-key (Phase 0)"
	}
	return fmt.Sprintf("%d authorized key(s)", len(allowed))
}

// ---- daemon ----------------------------------------------------------------

func cmdDaemon(g Globals, args []string) int {
	fs := newFlagSet("daemon")
	_ = fs.bool("foreground", false, "run in the foreground (default when launched directly)")
	serverTarget := fs.string("server", "", "server user@host:port to connect to")
	if code := fs.parse(args); code != 0 {
		return code
	}

	cfg, err := openConfig(g)
	if err != nil {
		return fail(1, err)
	}
	if *serverTarget != "" {
		if err := cfg.SetServer(*serverTarget); err != nil {
			return fail(1, err)
		}
	}
	sock, err := g.socketPath()
	if err != nil {
		return fail(1, err)
	}

	d, err := daemon.New(cfg, sock, g.Room)
	if err != nil {
		return fail(1, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := d.Run(ctx); err != nil {
		return fail(1, err)
	}
	return 0
}

// ---- join ------------------------------------------------------------------

func cmdJoin(g Globals, args []string) int {
	fs := newFlagSet("join")
	if code := fs.parse(args); code != 0 {
		return code
	}
	pos := fs.args()
	if len(pos) < 1 {
		return fail(2, errors.New("usage: room join <user@host:port>"))
	}
	target := pos[0]

	// Ensure the client identity key exists and print its fingerprint so it can
	// be allowlisted on the server (PROTOCOL §3: SSH key = identity).
	cfg, err := openConfig(g)
	if err != nil {
		return fail(1, err)
	}
	fp, err := cfg.Fingerprint()
	if err != nil {
		return fail(1, err)
	}
	line, _ := cfg.AuthorizedKeyLine()

	// Talk to the daemon (auto-spawn), telling it to connect.
	resp, code := roundtrip(g, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpJoin, Target: target})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr {
		return fail(resp.Code, errors.New(resp.Message))
	}

	fmt.Printf("joined %s (room %q)\n", target, effectiveRoom(g, cfg))
	fmt.Printf("device fingerprint: %s\n", fp)
	fmt.Printf("allowlist this device on the server (authorized_keys):\n  %s\n", strings.TrimSpace(line))
	return 0
}

func effectiveRoom(g Globals, cfg *config.Store) string {
	if g.Room != "" {
		return g.Room
	}
	if r := cfg.Get().Room; r != "" {
		return r
	}
	return "default"
}

// ---- send ------------------------------------------------------------------

func cmdSend(g Globals, args []string) int {
	fs := newFlagSet("send")
	asText := fs.bool("text", false, "force text")
	asImage := fs.bool("image", false, "force image (PNG/JPEG)")
	_ = fs.bool("auto", false, "sniff type (default)")
	if code := fs.parse(args); code != 0 {
		return code
	}

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail(1, err)
	}
	if len(data) == 0 {
		return fail(2, errors.New("send: empty stdin"))
	}
	force := ""
	if *asText {
		force = "text"
	} else if *asImage {
		force = "image"
	}

	resp, code := roundtrip(g, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpSend, Bytes: data, Force: force})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr {
		return fail(resp.Code, errors.New(resp.Message))
	}
	if resp.Code == 4 && !g.Quiet {
		fmt.Fprintf(os.Stderr, "room: warning: %s\n", resp.Message)
	}
	return 0
}

// ---- recv ------------------------------------------------------------------

func cmdRecv(g Globals, args []string) int {
	fs := newFlagSet("recv")
	follow := fs.bool("follow", false, "stream until interrupted")
	latestImage := fs.bool("latest-image", false, "return the latest image")
	emitPath := fs.bool("emit-path", false, "print the image file path")
	out := fs.string("out", "", "write image to this path")
	if code := fs.parse(args); code != 0 {
		return code
	}

	kind := "any"
	if *latestImage {
		kind = "image"
	}

	req := &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpRecv, Kind: kind, Follow: *follow}

	sock, err := g.socketPath()
	if err != nil {
		return fail(1, err)
	}
	conn, err := ipc.Dial(sock, true, g.spawnArgs())
	if err != nil {
		return fail(3, err)
	}
	defer conn.Close()
	if err := ipc.WriteReq(conn, req); err != nil {
		return fail(3, err)
	}

	if *follow {
		// Stream until the connection closes (Ctrl-C kills the client).
		for {
			resp, err := ipc.ReadResp(conn)
			if err != nil {
				return 0
			}
			printItem(resp, *emitPath || *latestImage, *out)
		}
	}

	resp, err := ipc.ReadResp(conn)
	if err != nil {
		return fail(3, err)
	}
	if resp.Kind == ipc.RespErr {
		return fail(resp.Code, errors.New(resp.Message))
	}
	printItem(resp, *emitPath || *latestImage, *out)
	return 0
}

// printItem renders one item to stdout: text inline; image as a file path
// (copied to --out first if requested).
func printItem(resp *ipc.Response, emitPath bool, out string) {
	if resp.Kind != ipc.RespItem || resp.Item == nil {
		return
	}
	env := &resp.Item.Envelope
	if env.Type == "text" {
		fmt.Println(env.Text)
		return
	}
	// image
	path := resp.Item.LocalPath
	if out != "" && path != "" {
		if err := copyFile(path, out); err == nil {
			path = out
		}
	}
	if emitPath || out != "" {
		fmt.Println(path)
	} else {
		fmt.Printf("[image %dx%d %s]\n", env.Blob.W, env.Blob.H, path)
	}
}

// ---- paste -----------------------------------------------------------------

func cmdPaste(g Globals, args []string) int {
	resp, code := roundtrip(g, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpPaste})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr {
		return fail(resp.Code, errors.New(resp.Message))
	}
	if !g.Quiet {
		fmt.Println("pasted latest item to clipboard")
	}
	return 0
}

// ---- status ----------------------------------------------------------------

func cmdStatus(g Globals, args []string) int {
	resp, code := roundtrip(g, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpStatus})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr || resp.Status == nil {
		return fail(1, errors.New("status unavailable"))
	}
	s := resp.Status
	if g.JSON {
		fmt.Printf(`{"fingerprint":%q,"device_name":%q,"room":%q,"server":%q,"connected":%t,"auto_copy":%q,"peers":%d,"buffer":%d,"clipboard":%t}`+"\n",
			s.Fingerprint, s.DeviceName, s.Room, s.Server, s.Connected, s.AutoCopy, s.Peers, s.Buffer, s.Clipboard)
		return 0
	}
	fmt.Printf("identity:   %s\n", s.Fingerprint)
	fmt.Printf("device:     %s\n", s.DeviceName)
	fmt.Printf("room:       %s\n", s.Room)
	fmt.Printf("server:     %s\n", s.Server)
	fmt.Printf("connected:  %t\n", s.Connected)
	fmt.Printf("auto_copy:  %s\n", s.AutoCopy)
	fmt.Printf("clipboard:  %s\n", clipboardLabel(s.Clipboard))
	fmt.Printf("buffer:     %d item(s)\n", s.Buffer)
	return 0
}

func clipboardLabel(ok bool) string {
	if ok {
		return "available"
	}
	return "unavailable"
}

// ---- config ----------------------------------------------------------------

func cmdConfig(g Globals, args []string) int {
	if len(args) < 2 {
		return fail(2, errors.New("usage: room config set KEY VALUE | room config get KEY"))
	}
	switch args[0] {
	case "set":
		if len(args) < 3 {
			return fail(2, errors.New("usage: room config set KEY VALUE"))
		}
		resp, code := roundtrip(g, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpConfigSet, Key: args[1], Value: args[2]})
		if code != 0 {
			return code
		}
		if resp.Kind == ipc.RespErr {
			return fail(resp.Code, errors.New(resp.Message))
		}
		if !g.Quiet {
			fmt.Printf("%s = %s\n", args[1], args[2])
		}
		return 0
	case "get":
		resp, code := roundtrip(g, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpConfigGet, Key: args[1]})
		if code != 0 {
			return code
		}
		if resp.Kind == ipc.RespErr {
			return fail(resp.Code, errors.New(resp.Message))
		}
		fmt.Println(resp.Value)
		return 0
	default:
		return fail(2, fmt.Errorf("config: unknown action %q", args[0]))
	}
}

// ---- tui --------------------------------------------------------------------

func cmdTui(g Globals, args []string) int {
	fs := newFlagSet("tui")
	if code := fs.parse(args); code != 0 {
		return code
	}
	sock, err := g.socketPath()
	if err != nil {
		return fail(1, err)
	}
	if err := tui.Run(tui.Options{
		SocketPath: sock,
		SpawnArgs:  g.spawnArgs(),
		Room:       g.Room,
	}); err != nil {
		return fail(1, err)
	}
	return 0
}

// ---- shared client plumbing ------------------------------------------------

// roundtrip dials the daemon (auto-spawning it), sends one request, and reads
// one response.
func roundtrip(g Globals, req *ipc.Request) (*ipc.Response, int) {
	sock, err := g.socketPath()
	if err != nil {
		return nil, fail(1, err)
	}
	conn, err := ipc.Dial(sock, true, g.spawnArgs())
	if err != nil {
		return nil, fail(3, err)
	}
	defer conn.Close()
	if err := ipc.WriteReq(conn, req); err != nil {
		return nil, fail(3, err)
	}
	resp, err := ipc.ReadResp(conn)
	if err != nil {
		return nil, fail(3, err)
	}
	return resp, 0
}

func openConfig(g Globals) (*config.Store, error) {
	dir, err := resolveConfigDir(g)
	if err != nil {
		return nil, err
	}
	return config.Open(dir)
}

func resolveConfigDir(g Globals) (string, error) {
	if g.ConfigDir != "" {
		return g.ConfigDir, nil
	}
	return config.DefaultDir()
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func fail(code int, err error) int {
	fmt.Fprintf(os.Stderr, "room: %v\n", err)
	return code
}

func usage() {
	fmt.Fprint(os.Stderr, `room — cross-platform clipboard hand-off (room-go, Phase 0)

Usage:
  room [global flags] <command> [command flags]

Global flags:
  --config-dir PATH   config/identity directory
  --socket PATH       IPC socket path
  --room NAME         room to join (default "default")
  --json              machine-readable output (status)
  -q, --quiet         quieter output
  -v, --verbose       debug logging

Commands:
  server   [--addr :2222] [--host-key PATH] [--authorized-keys FILE]
  daemon   [--foreground] [--server user@host:port]
  join     <user@host:port>
  send     [--text|--image|--auto]        (reads stdin)
  recv     [--follow] [--latest-image --emit-path] [--out PATH]
  paste
  tui                                     (messenger-style chat)
  status   [--json]
  config   set KEY VALUE | get KEY
`)
}
