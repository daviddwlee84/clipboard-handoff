package daemon

import (
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/clip"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/ipc"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

// handleConn serves one IPC connection. Most ops are request/response; recv
// --follow and subscribe hold the connection open and stream events.
func (d *Daemon) handleConn(c net.Conn) {
	defer c.Close()
	req, err := ipc.ReadReq(c)
	if err != nil {
		return
	}
	if req.IPCVersion != ipc.IPCVersion {
		_ = ipc.WriteResp(c, errResp(2, "IPC version mismatch: client "+itoa(req.IPCVersion)+" daemon "+itoa(ipc.IPCVersion)))
		return
	}

	switch req.Op {
	case ipc.OpPing:
		_ = ipc.WriteResp(c, okResp())
	case ipc.OpSend:
		d.handleSend(c, req)
	case ipc.OpRecv:
		d.handleRecv(c, req)
	case ipc.OpPaste:
		d.handlePaste(c)
	case ipc.OpCopy:
		d.handleCopy(c, req)
	case ipc.OpClear:
		d.handleClear(c, req)
	case ipc.OpSubscribe:
		d.handleSubscribe(c, "any")
	case ipc.OpStatus, ipc.OpPeers:
		d.handleStatus(c)
	case ipc.OpConfigSet:
		d.handleConfigSet(c, req)
	case ipc.OpConfigGet:
		d.handleConfigGet(c, req)
	case ipc.OpDaemonStop:
		d.handleDaemonStop(c, req)
	default:
		_ = ipc.WriteResp(c, errResp(2, "unknown op: "+req.Op))
	}
}

func (d *Daemon) handleSend(c net.Conn, req *ipc.Request) {
	if len(req.Bytes) == 0 {
		_ = ipc.WriteResp(c, errResp(2, "empty payload"))
		return
	}

	env := &wire.Envelope{
		V:          wire.Version,
		MsgID:      ulid.Make().String(),
		Sender:     d.fp,
		DeviceName: d.deviceName,
		TS:         uint64(time.Now().UnixMilli()),
	}

	// Determine the type (PROTOCOL §2). --text/--image/--file force it; --auto
	// (default) sniffs: PNG/JPEG magic → image; valid UTF-8 → text; else file.
	switch req.Force {
	case "text":
		env.Type = wire.TypeText
		env.Mime = wire.MimeText
		env.Text = string(req.Bytes)
	case "image":
		if serr := fillImage(env, req.Bytes, req.Name); serr != nil {
			_ = ipc.WriteResp(c, errResp(2, serr.Error()))
			return
		}
	case "file":
		fillFile(env, req.Bytes, req.Name)
	default: // auto
		kind, serr := wire.Sniff(req.Bytes)
		switch {
		case serr == nil && (kind == wire.KindPNG || kind == wire.KindJPEG):
			if ferr := fillImage(env, req.Bytes, req.Name); ferr != nil {
				_ = ipc.WriteResp(c, errResp(1, ferr.Error()))
				return
			}
		case serr == nil && kind == wire.KindText:
			env.Type = wire.TypeText
			env.Mime = wire.MimeText
			env.Text = string(req.Bytes)
		default:
			// Binary, non-image → file with a best-effort mime (PROTOCOL §2).
			fillFile(env, req.Bytes, req.Name)
		}
	}

	// SPEC §3: mark our own msg_id seen before broadcasting so a transport that
	// echoes published messages back to us (gossipsub self-delivery) is
	// suppressed, and we never re-ingest what we sent.
	d.dedupe.Seen(env.MsgID)

	// Give discovery a brief window so the first send right after startup isn't
	// dropped before a peer is connected/subscribed (mDNS + gossipsub warmup).
	// Returns immediately in steady state (peers already present).
	d.waitForPeers(5 * time.Second)

	if err := d.tr.Broadcast(env); err != nil {
		// Broadcast failed outright.
		_ = ipc.WriteResp(c, errResp(1, err.Error()))
		return
	}
	log.Printf("sent type=%s msg_id=%s peers=%d", env.Type, env.MsgID, len(d.tr.Peers()))

	if len(d.tr.Peers()) == 0 {
		// SPEC exit 4: no peers is a warning, not a failure — still exit 0-ish.
		_ = ipc.WriteResp(c, &ipc.Response{IPCVersion: ipc.IPCVersion, Kind: ipc.RespOK, Code: 4, Message: "no peers connected yet"})
		return
	}
	_ = ipc.WriteResp(c, okResp())
}

// fillImage populates env as an image: sniff (must be PNG/JPEG), transcode to
// the canonical PNG wire format, and set the blob + inline bytes + filename.
func fillImage(env *wire.Envelope, data []byte, filename string) error {
	kind, serr := wire.Sniff(data)
	if serr != nil || (kind != wire.KindPNG && kind != wire.KindJPEG) {
		return errors.New("not a supported image (PNG/JPEG)")
	}
	png, perr := wire.ToPNG(kind, data) // canonical wire format is PNG
	if perr != nil {
		return perr
	}
	w, h, derr := wire.PNGDims(png)
	if derr != nil {
		return fmt.Errorf("decode png dims: %w", derr)
	}
	env.Type = wire.TypeImage
	env.Mime = wire.MimePNG
	env.Filename = filename
	env.Blob = &wire.Blob{Hash: wire.HashHex(png), Size: uint64(len(png)), W: w, H: h}
	env.BlobData = png // Phase 0: inline (PROTOCOL §1 blob_data)
	return nil
}

// fillFile populates env as an arbitrary file: best-effort mime from the
// extension, inline content-addressed bytes, and the advisory filename.
func fillFile(env *wire.Envelope, data []byte, filename string) {
	env.Type = wire.TypeFile
	env.Mime = wire.MimeForFile(filename)
	env.Filename = filename
	env.Blob = &wire.Blob{Hash: wire.HashHex(data), Size: uint64(len(data))}
	env.BlobData = data // Phase 0: inline (PROTOCOL §1 blob_data)
}

func (d *Daemon) handleRecv(c net.Conn, req *ipc.Request) {
	kind := req.Kind
	if kind == "" {
		kind = "any"
	}
	if req.Follow {
		d.handleSubscribe(c, kind)
		return
	}
	// Non-follow: return the latest buffered item of this kind, else block for
	// the next one (SPEC §2).
	if item := d.latest(kind); item != nil {
		_ = ipc.WriteResp(c, itemResp(item))
		return
	}
	ch := d.subscribe()
	defer d.unsubscribe(ch)
	if item := waitFor(ch, kind, 0); item != nil {
		_ = ipc.WriteResp(c, itemResp(item))
		return
	}
	_ = ipc.WriteResp(c, errResp(5, "nothing to receive"))
}

func (d *Daemon) handleSubscribe(c net.Conn, kind string) {
	ch := d.subscribe()
	defer d.unsubscribe(ch)
	for {
		select {
		case <-d.done:
			return
		case item := <-ch:
			if !matchesKind(item, kind) {
				continue
			}
			if err := ipc.WriteResp(c, itemResp(item)); err != nil {
				return // client disconnected
			}
		}
	}
}

func (d *Daemon) handlePaste(c net.Conn) {
	item := d.latest("any")
	if item == nil {
		_ = ipc.WriteResp(c, errResp(5, "nothing to paste"))
		return
	}
	if err := d.writeClipboard(item); err != nil {
		_ = ipc.WriteResp(c, errResp(1, "clipboard: "+err.Error()))
		return
	}
	_ = ipc.WriteResp(c, okResp())
}

// handleCopy writes client-supplied bytes to the OS clipboard (TUI `y` on a
// highlighted bubble). Unlike paste (which copies the daemon's latest buffered
// item), the client hands over the exact payload — so any bubble, including the
// user's own outgoing text, can be copied. Records the content hash for echo
// suppression (SPEC §3 rule 2), same as writeClipboard.
func (d *Daemon) handleCopy(c net.Conn, req *ipc.Request) {
	if len(req.Bytes) == 0 {
		_ = ipc.WriteResp(c, errResp(2, "empty payload"))
		return
	}
	var err error
	if req.Force == "image" {
		err = clip.WritePNG(req.Bytes)
	} else {
		err = clip.WriteText(string(req.Bytes))
	}
	if err != nil {
		_ = ipc.WriteResp(c, errResp(1, "clipboard: "+err.Error()))
		return
	}
	d.mu.Lock()
	d.lastCopy = wire.HashHex(req.Bytes)
	d.mu.Unlock()
	_ = ipc.WriteResp(c, okResp())
}

func (d *Daemon) handleStatus(c net.Conn) {
	s := d.cfg.Get()
	clipState := "unavailable"
	if clip.Available() {
		clipState = "available"
	}
	st := &ipc.Status{
		Identity:   d.fp,
		DeviceName: d.deviceName,
		Room:       d.room,
		Transport:  d.transportTag,
		AutoCopy:   s.AutoCopy,
		Clipboard:  clipState,
		Peers:      d.tr.Peers(),
		Buffer:     d.bufferLen(),
	}
	_ = ipc.WriteResp(c, &ipc.Response{IPCVersion: ipc.IPCVersion, Kind: ipc.RespStatus, Status: st})
}

func (d *Daemon) handleConfigSet(c net.Conn, req *ipc.Request) {
	if err := d.cfg.SetKey(req.Key, req.Value); err != nil {
		_ = ipc.WriteResp(c, errResp(2, err.Error()))
		return
	}
	switch req.Key {
	case "device_name":
		d.deviceName = req.Value
	case "text_file":
		// Capture the new sink's session-start offset so a later clear --all
		// truncates to here, not to before it was configured (SPEC §8).
		d.recordTextBaseline(req.Value)
	}
	_ = ipc.WriteResp(c, okResp())
}

func (d *Daemon) handleConfigGet(c net.Conn, req *ipc.Request) {
	v, err := d.cfg.GetKey(req.Key)
	if err != nil {
		_ = ipc.WriteResp(c, errResp(2, err.Error()))
		return
	}
	_ = ipc.WriteResp(c, &ipc.Response{IPCVersion: ipc.IPCVersion, Kind: ipc.RespValue, Value: v})
}

// waitForPeers blocks until at least one peer is connected/subscribed or max
// elapses. It returns immediately in steady state (peers already present); on
// the first send after startup it adds a small grace once a peer appears so the
// transport's subscription/mesh can settle before we publish.
func (d *Daemon) waitForPeers(max time.Duration) bool {
	if len(d.tr.Peers()) > 0 {
		return true
	}
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if len(d.tr.Peers()) > 0 {
			time.Sleep(300 * time.Millisecond)
			return true
		}
	}
	return false
}

// writeClipboard writes an item to the OS clipboard and records its content
// hash for echo suppression (SPEC §3 rule 2). A file has no image clipboard
// form — its local path is copied as text instead (SPEC §2 `paste`).
func (d *Daemon) writeClipboard(item *ipc.Item) error {
	isImage, data := clipboardPayload(item)
	var err error
	if isImage {
		err = clip.WritePNG(data)
	} else {
		err = clip.WriteText(string(data))
	}
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.lastCopy = wire.HashHex(data)
	d.mu.Unlock()
	return nil
}

// clipboardPayload decides what bytes a paste places on the clipboard, and
// whether they are an image (SPEC §2/§3): text → the text; image → the PNG
// bytes; file → its materialized path, copied as clipboard text.
func clipboardPayload(item *ipc.Item) (isImage bool, data []byte) {
	env := &item.Envelope
	switch env.Type {
	case wire.TypeImage:
		return true, env.BlobData
	case wire.TypeFile:
		return false, []byte(item.LocalPath)
	default:
		return false, []byte(env.Text)
	}
}

// handleClear reverts this session's data (SPEC §8). Transient by default; --all
// also reverts session sink writes. The client owns the confirmation prompt.
func (d *Daemon) handleClear(c net.Conn, req *ipc.Request) {
	d.clear(req.All)
	log.Printf("clear (all=%v) applied via IPC", req.All)
	_ = ipc.WriteResp(c, okResp())
}

// handleDaemonStop applies the resolved clear_on_exit policy on shutdown and
// tears the daemon down (SPEC §8). The client resolves "ask" (TTY prompt →
// t/a/n, else transient) and passes the concrete policy in req.Value.
func (d *Daemon) handleDaemonStop(c net.Conn, req *ipc.Request) {
	d.mu.Lock()
	d.stopPolicy = req.Value
	d.mu.Unlock()
	_ = ipc.WriteResp(c, okResp())
	log.Printf("daemon stop requested (policy=%q)", req.Value)
	if d.cancel != nil {
		d.cancel() // triggers Run to return; applyExitPolicy runs on the way out
	}
}

// ---- small helpers ----

func okResp() *ipc.Response { return &ipc.Response{IPCVersion: ipc.IPCVersion, Kind: ipc.RespOK} }
func errResp(code int, msg string) *ipc.Response {
	return &ipc.Response{IPCVersion: ipc.IPCVersion, Kind: ipc.RespErr, Code: code, Message: msg}
}
func itemResp(item *ipc.Item) *ipc.Response {
	return &ipc.Response{IPCVersion: ipc.IPCVersion, Kind: ipc.RespItem, Item: item}
}

func itoa(i int) string { return strconv.Itoa(i) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
