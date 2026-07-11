// Package daemon is the resident, transport-agnostic client agent for the
// experiments track (SPEC §1). It owns the local OS clipboard, a ring buffer of
// received items, msg_id dedupe + last-written-hash echo suppression, the
// notify/on/off auto-copy policy, and a local Unix-socket IPC (PROTOCOL §4).
// The network is abstracted behind transport.Transport, so each probe supplies
// only a transport (quic-go+mDNS, libp2p gossipsub+mDNS, ...).
package daemon

import (
	"context"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/clip"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/config"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/ipc"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/transport"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

const bufferCap = 64

// Daemon is the resident agent.
type Daemon struct {
	cfg          *config.Store
	socketPath   string
	room         string
	transportTag string // label for `status` (e.g. "quic+mdns")

	tr         transport.Transport
	fp         string
	deviceName string
	dedupe     *Dedupe

	mu       sync.Mutex
	buffer   []*ipc.Item // ring buffer, oldest first
	subs     map[chan *ipc.Item]struct{}
	lastCopy string // BLAKE3 of the last value we wrote to our own clipboard (SPEC §3 rule 2)

	// Session state (SPEC §8): tracked so `clear` / exit is precise and never
	// destructive beyond this session.
	sessionSaveFiles []string         // files this session wrote into save_dir
	textBaseline     map[string]int64 // text_file path -> size at session start (truncate target)
	gotReceive       bool             // received anything this session
	stopPolicy       string           // resolved clear_on_exit to apply on shutdown ("" = read config)

	cancel context.CancelFunc // triggers shutdown (daemon stop)
	done   chan struct{}
}

// New builds a daemon over the given transport. room overrides the configured
// room when non-empty; transportTag labels the transport in `status`.
func New(cfg *config.Store, socketPath, room, transportTag string, tr transport.Transport) *Daemon {
	settings := cfg.Get()
	if room == "" {
		room = settings.Room
	}
	if room == "" {
		room = "default"
	}
	return &Daemon{
		cfg:          cfg,
		socketPath:   socketPath,
		room:         room,
		transportTag: transportTag,
		tr:           tr,
		fp:           tr.Identity(),
		deviceName:   settings.DeviceName,
		dedupe:       NewDedupe(4096),
		subs:         make(map[chan *ipc.Item]struct{}),
		textBaseline: make(map[string]int64),
		done:         make(chan struct{}),
	}
}

// Run starts the transport and the IPC listener, then blocks until ctx is
// cancelled (signal) or `daemon stop` triggers shutdown. On exit it applies the
// clear_on_exit policy (SPEC §8).
func (d *Daemon) Run(ctx context.Context) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	defer cancel()

	if err := os.MkdirAll(d.cfg.BlobDir(), 0o700); err != nil {
		return err
	}
	// Record the text_file's session-start offset now so `clear --all` only
	// reverts text appended during this session (SPEC §8).
	d.recordTextBaseline(d.cfg.Get().TextFile)
	// On any shutdown, apply the exit policy (transient/all/never) once.
	defer d.applyExitPolicy()

	d.tr.OnReceive(d.ingest)
	if err := d.tr.Start(ctx); err != nil {
		return err
	}
	defer d.tr.Close()

	// Fresh socket: remove a stale one first.
	_ = os.Remove(d.socketPath)
	ln, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(d.socketPath)

	log.Printf("daemon listening: socket=%s room=%q id=%s transport=%s",
		d.socketPath, d.room, short(d.fp), d.transportTag)

	go func() {
		<-ctx.Done()
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

// ingest processes an envelope received from a peer (SPEC §3). Registered with
// the transport via OnReceive.
func (d *Daemon) ingest(env *wire.Envelope) {
	// Rule 1: dedupe by msg_id (also suppresses our own echoed publishes).
	if d.dedupe.Seen(env.MsgID) {
		return
	}
	// Rule 3 guard: never treat our own sender id as a received item.
	if env.Sender == d.fp {
		return
	}

	item := &ipc.Item{Envelope: *env}

	if env.Type == wire.TypeImage || env.Type == wire.TypeFile {
		// Integrity check (PROTOCOL §1): blob.hash MUST be verified on receipt.
		if env.Blob == nil || wire.HashHex(env.BlobData) != env.Blob.Hash {
			log.Printf("blob integrity check failed; dropping msg_id=%s", env.MsgID)
			return
		}
		// Materialize the bytes to the blob cache (also the recv --emit-path temp
		// file). Name by hash; keep a usable extension so it opens externally.
		name := env.Blob.Hash
		if env.Type == wire.TypeImage {
			name += ".png"
		} else if ext := filepath.Ext(env.Filename); ext != "" {
			name += ext
		}
		path := filepath.Join(d.cfg.BlobDir(), name)
		if err := os.WriteFile(path, env.BlobData, 0o600); err != nil {
			log.Printf("write blob: %v", err)
			return
		}
		item.LocalPath = path
	}

	d.mu.Lock()
	d.buffer = append(d.buffer, item)
	if len(d.buffer) > bufferCap {
		d.buffer = d.buffer[len(d.buffer)-bufferCap:]
	}
	d.gotReceive = true
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

	// Additive sinks (SPEC §3): clipboard (per auto_copy) plus, if configured,
	// the folder/append-file sinks. Independent of one another.
	d.applyAutoCopy(item)
	d.routeSinks(item)
}

// applyAutoCopy enforces SPEC §3. Phase 0 trusts all senders (TOFU allowlist is
// Phase 1); the mode still governs whether we touch the clipboard.
func (d *Daemon) applyAutoCopy(item *ipc.Item) {
	mode := d.cfg.Get().AutoCopy
	env := &item.Envelope
	label := env.DeviceName
	if label == "" {
		label = short(env.Sender)
	}
	switch mode {
	case "on":
		var err error
		var hash string
		switch env.Type {
		case wire.TypeText:
			hash = wire.HashHex([]byte(env.Text))
			err = clip.WriteText(env.Text)
		case wire.TypeImage:
			hash = env.Blob.Hash
			err = clip.WritePNG(env.BlobData)
		case wire.TypeFile:
			// A file has no clipboard image form (SPEC §3); it lands in the folder
			// sink instead. Nothing to auto-copy.
			log.Printf("received file (auto_copy on; no clipboard form) from=%s name=%q", label, env.Filename)
			return
		}
		if err != nil {
			log.Printf("auto_copy on: clipboard write failed: %v", err)
			return
		}
		d.mu.Lock()
		d.lastCopy = hash // SPEC §3 rule 2: remember what we wrote
		d.mu.Unlock()
		log.Printf("auto-copied to clipboard from=%s type=%s", label, env.Type)
	case "off":
		// never touch the clipboard
	default: // "notify"
		d.notify(env, label)
	}
}

// notify is the Phase 0 notify-first surface: a log line (desktop notification
// is a Phase 1 nicety). It does NOT write the clipboard.
func (d *Daemon) notify(env *wire.Envelope, label string) {
	preview := env.Text
	switch {
	case env.Type == wire.TypeImage && env.Blob != nil:
		preview = "image " + itoa(int(env.Blob.W)) + "x" + itoa(int(env.Blob.H))
	case env.Type == wire.TypeFile && env.Blob != nil:
		preview = "file " + env.Filename + " (" + itoa(int(env.Blob.Size)) + "B)"
	}
	log.Printf("received (notify) from=%s type=%s preview=%q", label, env.Type, truncate(preview, 40))
}

// latest returns a copy of the newest buffered item matching kind, or nil.
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
	case wire.TypeFile:
		return item.Envelope.Type == wire.TypeFile
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

func short(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "…"
}
