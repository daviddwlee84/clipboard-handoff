package daemon

import (
	"net"
	"strconv"
	"time"

	"github.com/charmbracelet/log"
	"github.com/oklog/ulid/v2"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/clip"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
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
	case ipc.OpSubscribe:
		d.handleSubscribe(c, "any")
	case ipc.OpStatus:
		d.handleStatus(c)
	case ipc.OpConfigSet:
		d.handleConfigSet(c, req)
	case ipc.OpConfigGet:
		d.handleConfigGet(c, req)
	case ipc.OpJoin:
		d.handleJoin(c, req)
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

	// Decide text vs image: honor --text/--image force, else sniff.
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
		env.BlobData = png // Phase 0: inline
	}

	frame, err := wire.Marshal(env)
	if err != nil {
		_ = ipc.WriteResp(c, errResp(1, err.Error()))
		return
	}
	if err := d.sendFrame(frame); err != nil {
		// Not connected to a server / no peers: SPEC exit 4 is a warning, not
		// a failure. Report code 4 so the client can warn but still exit 0-ish.
		_ = ipc.WriteResp(c, &ipc.Response{IPCVersion: ipc.IPCVersion, Kind: ipc.RespOK, Code: 4, Message: err.Error()})
		return
	}
	log.Info("sent", "type", env.Type, "msg_id", env.MsgID)
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
	// the next one (SPEC §2: "waits for the next single item (or returns the
	// latest buffered one)").
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

// handleCopy places a chosen item on the OS clipboard via the daemon (SPEC §4
// TUI `y`), keeping the daemon the sole clipboard owner. Selection order:
// an explicit MsgID (a received/buffered item) → inline Bytes (the TUI's own
// outgoing text) → the latest buffered item.
func (d *Daemon) handleCopy(c net.Conn, req *ipc.Request) {
	if req.MsgID != "" {
		item := d.findByID(req.MsgID)
		if item == nil {
			_ = ipc.WriteResp(c, errResp(5, "item not in buffer"))
			return
		}
		if err := d.writeClipboard(item); err != nil {
			_ = ipc.WriteResp(c, errResp(1, "clipboard: "+err.Error()))
			return
		}
		_ = ipc.WriteResp(c, okResp())
		return
	}
	if len(req.Bytes) > 0 {
		// Inline text (the TUI copying one of its own sent messages). Record the
		// content hash for echo suppression, exactly like a normal clipboard write.
		hash := wire.HashHex(req.Bytes)
		if err := clip.WriteText(string(req.Bytes)); err != nil {
			_ = ipc.WriteResp(c, errResp(1, "clipboard: "+err.Error()))
			return
		}
		d.mu.Lock()
		d.lastCopy = hash
		d.mu.Unlock()
		_ = ipc.WriteResp(c, okResp())
		return
	}
	d.handlePaste(c) // no selector: fall back to the latest item
}

func (d *Daemon) handleStatus(c net.Conn) {
	s := d.cfg.Get()
	st := &ipc.Status{
		Fingerprint: d.fp,
		DeviceName:  d.deviceName,
		Room:        d.room,
		Server:      d.serverTarget(),
		Connected:   d.isConnected(),
		AutoCopy:    s.AutoCopy,
		Peers:       0, // server does not push a peer list in Phase 0
		Buffer:      d.bufferLen(),
		Clipboard:   clip.Available(),
	}
	if st.Server == "" {
		st.Server = s.Server
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

func (d *Daemon) handleJoin(c net.Conn, req *ipc.Request) {
	if req.Target == "" {
		_ = ipc.WriteResp(c, errResp(2, "join requires a target user@host:port"))
		return
	}
	if err := d.cfg.SetServer(req.Target); err != nil {
		_ = ipc.WriteResp(c, errResp(1, err.Error()))
		return
	}
	// (Re)connect: if already connected to a different target the old loop
	// keeps its session; for Phase 0 we simply start a loop for the new target
	// if not already connected to it.
	if d.serverTarget() != req.Target {
		go d.connectLoop(req.Target)
	}
	_ = ipc.WriteResp(c, okResp())
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
