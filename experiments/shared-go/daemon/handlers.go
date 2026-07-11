package daemon

import (
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
	case ipc.OpSubscribe:
		d.handleSubscribe(c, "any")
	case ipc.OpStatus, ipc.OpPeers:
		d.handleStatus(c)
	case ipc.OpConfigSet:
		d.handleConfigSet(c, req)
	case ipc.OpConfigGet:
		d.handleConfigGet(c, req)
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

	asImage := false
	switch req.Force {
	case "text":
		env.Type = wire.TypeText
		env.Mime = wire.MimeText
		env.Text = string(req.Bytes)
	case "image":
		asImage = true
	default:
		kind, serr := wire.Sniff(req.Bytes)
		if serr != nil {
			_ = ipc.WriteResp(c, errResp(2, serr.Error()))
			return
		}
		if kind == wire.KindText {
			env.Type = wire.TypeText
			env.Mime = wire.MimeText
			env.Text = string(req.Bytes)
		} else {
			asImage = true
		}
	}

	if asImage {
		kind, serr := wire.Sniff(req.Bytes)
		if serr != nil || (kind != wire.KindPNG && kind != wire.KindJPEG) {
			_ = ipc.WriteResp(c, errResp(2, "not a supported image (PNG/JPEG)"))
			return
		}
		png, perr := wire.ToPNG(kind, req.Bytes) // canonical wire format is PNG
		if perr != nil {
			_ = ipc.WriteResp(c, errResp(1, perr.Error()))
			return
		}
		w, h, derr := wire.PNGDims(png)
		if derr != nil {
			_ = ipc.WriteResp(c, errResp(1, "decode png dims: "+derr.Error()))
			return
		}
		env.Type = wire.TypeImage
		env.Mime = wire.MimePNG
		env.Blob = &wire.Blob{Hash: wire.HashHex(png), Size: uint64(len(png)), W: w, H: h}
		env.BlobData = png // Phase 0: inline (PROTOCOL §1 blob_data)
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

func (d *Daemon) handleStatus(c net.Conn) {
	s := d.cfg.Get()
	st := &ipc.Status{
		Identity:   d.fp,
		DeviceName: d.deviceName,
		Room:       d.room,
		Transport:  d.transportTag,
		AutoCopy:   s.AutoCopy,
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
	if req.Key == "device_name" {
		d.deviceName = req.Value
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
// hash for echo suppression (SPEC §3 rule 2).
func (d *Daemon) writeClipboard(item *ipc.Item) error {
	env := &item.Envelope
	var hash string
	var err error
	if env.Type == wire.TypeText {
		hash = wire.HashHex([]byte(env.Text))
		err = clip.WriteText(env.Text)
	} else {
		hash = env.Blob.Hash
		err = clip.WritePNG(env.BlobData)
	}
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.lastCopy = hash
	d.mu.Unlock()
	return nil
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
