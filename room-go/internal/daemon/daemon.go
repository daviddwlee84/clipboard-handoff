// Package daemon is the resident client agent (SPEC §1): it holds the
// persistent SSH connection to the room server, owns the local OS clipboard,
// keeps a ring buffer of received items, and serves thin clients over a local
// Unix-socket IPC (PROTOCOL §4). Received envelopes are deduped, integrity-
// checked, buffered, and — per auto_copy — surfaced or copied.
package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	gossh "golang.org/x/crypto/ssh"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/config"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

const bufferCap = 64

// Daemon is the resident agent.
type Daemon struct {
	cfg        *config.Store
	socketPath string
	room       string
	signer     gossh.Signer
	fp         string
	deviceName string

	conn   conn
	dedupe *Dedupe

	mu       sync.Mutex
	buffer   []*ipc.Item // ring buffer, oldest first
	subs     map[chan *ipc.Item]struct{}
	lastCopy string // BLAKE3 of the last value we wrote to our own clipboard (SPEC §3 rule 2)

	sess *session // per-session sink/transient tracking for `clear` (SPEC §8)

	done     chan struct{}
	stopReq  chan struct{} // closed by `daemon stop` to trigger shutdown
	stopOnce sync.Once
}

// New builds a daemon. room overrides the configured room when non-empty.
func New(cfg *config.Store, socketPath, room string) (*Daemon, error) {
	signer, err := cfg.Signer()
	if err != nil {
		return nil, err
	}
	settings := cfg.Get()
	if room == "" {
		room = settings.Room
	}
	if room == "" {
		room = "default"
	}
	d := &Daemon{
		cfg:        cfg,
		socketPath: socketPath,
		room:       room,
		signer:     signer,
		fp:         gossh.FingerprintSHA256(signer.PublicKey()),
		deviceName: settings.DeviceName,
		dedupe:     NewDedupe(4096),
		subs:       make(map[chan *ipc.Item]struct{}),
		sess:       newSession(settings.TextFile),
		done:       make(chan struct{}),
		stopReq:    make(chan struct{}),
	}
	return d, nil
}

// Run starts the IPC listener and (if a server is configured) the connection
// loop, then blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(d.cfg.BlobDir(), 0o700); err != nil {
		return err
	}

	// Fresh socket: remove a stale one first.
	_ = os.Remove(d.socketPath)
	ln, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(d.socketPath)

	log.Info("daemon listening", "socket", d.socketPath, "room", d.room, "fp", d.fp)

	if target := d.cfg.Get().Server; target != "" {
		go d.connectLoop(target)
	}

	// Shutdown watcher: either a SIGTERM/SIGINT (ctx) or an IPC `daemon stop`.
	// A `daemon stop` has already applied its resolved clear scope in the
	// handler; a signal applies the clear_on_exit policy non-interactively here
	// (SPEC §8: SIGTERM → policy, ask falls back to transient).
	go func() {
		select {
		case <-ctx.Done():
			d.applyClearOnExit(d.cfg.Get().ClearOnExit, false)
		case <-d.stopReq:
			// clear already applied by handleDaemonStop
		}
		close(d.done)
		ln.Close()
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-d.done:
				return nil
			default:
				return err
			}
		}
		go d.handleConn(c)
	}
}

// ingest processes an envelope received from the server.
func (d *Daemon) ingest(env *wire.Envelope) {
	// Rule 1: dedupe by msg_id.
	if d.dedupe.Seen(env.MsgID) {
		return
	}

	item := &ipc.Item{Envelope: *env}

	// Image and file are content-addressed blobs: verify BLAKE3 (PROTOCOL §1)
	// and materialize into the transient blob cache so `recv --emit-path`,
	// `paste`, and the folder sink have bytes/paths to work with.
	if env.Type == wire.TypeImage || env.Type == wire.TypeFile {
		if env.Blob == nil || wire.HashHex(env.BlobData) != env.Blob.Hash {
			log.Warn("blob integrity check failed; dropping", "msg_id", env.MsgID, "type", env.Type)
			return
		}
		name := env.Blob.Hash
		if env.Type == wire.TypeImage {
			name += ".png"
		} else {
			name += fileExt(env.Filename)
		}
		path := filepath.Join(d.cfg.BlobDir(), name)
		if err := os.WriteFile(path, env.BlobData, 0o600); err != nil {
			log.Warn("write blob", "err", err)
			return
		}
		item.LocalPath = path
	}

	d.mu.Lock()
	d.buffer = append(d.buffer, item)
	if len(d.buffer) > bufferCap {
		d.buffer = d.buffer[len(d.buffer)-bufferCap:]
	}
	subs := make([]chan *ipc.Item, 0, len(d.subs))
	for ch := range d.subs {
		subs = append(subs, ch)
	}
	d.mu.Unlock()

	// Notify subscribers (recv --follow / tui).
	for _, ch := range subs {
		select {
		case ch <- item:
		default: // slow subscriber; skip rather than block ingest
		}
	}

	// Additive sinks (SPEC §3): folder/append-file, independent of auto_copy.
	d.routeSinks(item)
	// Clipboard sink, governed by auto_copy.
	d.applyAutoCopy(item)
}

// applyAutoCopy enforces the clipboard sink of SPEC §3. Phase 0 trusts all
// senders (allowlist/TOFU is Phase 1); the mode still governs whether we touch
// the clipboard. A `file` is never auto-copied (it has no clipboard image
// form); `paste`/`y` copy its path as text on demand.
func (d *Daemon) applyAutoCopy(item *ipc.Item) {
	mode := d.cfg.Get().AutoCopy
	env := &item.Envelope
	label := env.DeviceName
	if label == "" {
		label = env.Sender
	}
	switch mode {
	case "on":
		if env.Type == wire.TypeFile {
			// files never hit the clipboard automatically (SPEC §3)
			return
		}
		if err := d.writeClipboard(item); err != nil {
			log.Warn("auto_copy on: clipboard write failed", "err", err)
			return
		}
		log.Info("auto-copied to clipboard", "from", label, "type", env.Type)
	case "off":
		// never touch the clipboard
	default: // "notify"
		// Surface a toast/notification but do NOT write the clipboard.
		d.notify(env, label)
	}
}

// notify is the Phase 0 notify-first surface: a log line + a Toast to any
// subscriber. A desktop notification is a Phase 1 nicety.
func (d *Daemon) notify(env *wire.Envelope, label string) {
	preview := env.Text
	switch env.Type {
	case wire.TypeImage:
		if env.Blob != nil {
			preview = "image " + itoa(int(env.Blob.W)) + "x" + itoa(int(env.Blob.H))
		}
	case wire.TypeFile:
		preview = "file " + fileLabel(env)
	}
	log.Info("received (notify)", "from", label, "type", env.Type, "preview", truncate(preview, 40))
}

// latest returns the newest buffered item matching kind (any|text|image), or
// nil. It also returns a copy so callers don't race the buffer.
func (d *Daemon) latest(kind string) *ipc.Item {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.buffer) - 1; i >= 0; i-- {
		if matchesKind(d.buffer[i], kind) {
			it := *d.buffer[i]
			return &it
		}
	}
	return nil
}

func (d *Daemon) bufferLen() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.buffer)
}

// findByID returns a copy of the buffered item with the given msg_id, or nil.
// Used by the TUI `y` copy action to place a specific received item on the
// clipboard through the daemon.
func (d *Daemon) findByID(id string) *ipc.Item {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.buffer) - 1; i >= 0; i-- {
		if d.buffer[i].Envelope.MsgID == id {
			it := *d.buffer[i]
			return &it
		}
	}
	return nil
}

func (d *Daemon) subscribe() chan *ipc.Item {
	ch := make(chan *ipc.Item, 16)
	d.mu.Lock()
	d.subs[ch] = struct{}{}
	d.mu.Unlock()
	return ch
}

func (d *Daemon) unsubscribe(ch chan *ipc.Item) {
	d.mu.Lock()
	delete(d.subs, ch)
	d.mu.Unlock()
}

func matchesKind(item *ipc.Item, kind string) bool {
	switch kind {
	case "", "any":
		return true
	case wire.TypeText:
		return item.Envelope.Type == wire.TypeText
	case wire.TypeImage:
		return item.Envelope.Type == wire.TypeImage
	default:
		return true
	}
}

// waitFor blocks until an item matching kind arrives on ch, ctx is done, or
// timeout elapses. Used by non-follow recv when the buffer is empty.
func waitFor(ch chan *ipc.Item, kind string, timeout time.Duration) *ipc.Item {
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	for {
		select {
		case item := <-ch:
			if matchesKind(item, kind) {
				return item
			}
		case <-timer:
			return nil
		}
	}
}
