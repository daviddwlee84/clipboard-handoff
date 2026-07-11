package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
)

// daemonClient is the narrow slice of daemon IPC the TUI model needs. It is an
// interface so the model's Update can be unit-tested against a fake with no
// live daemon (see tui_test.go).
type daemonClient interface {
	// Status fetches the daemon state snapshot for the header.
	Status() (*ipc.Status, error)
	// SendText broadcasts a text item. warn is a non-fatal notice (e.g. "no
	// peers connected" — the item is still buffered), err is a real failure.
	SendText(text string) (warn string, err error)
	// Copy asks the daemon (the clipboard owner) to place an item on the OS
	// clipboard: a received item by msgID, else fallbackText inline, else the
	// latest buffered item.
	Copy(msgID, fallbackText string) error
	// Clear runs the session clear (SPEC §8) on the daemon: transient by
	// default; all also reverts this session's sink writes.
	Clear(all bool) error
}

// Options configure how the TUI reaches the local daemon.
type Options struct {
	SocketPath string
	SpawnArgs  []string // global flags so an auto-spawned daemon inherits config
	Room       string
}

// ipcClient is the real daemonClient over the Unix-socket IPC. Each call is a
// fresh connection (the daemon serves one request per connection); auto-spawn
// is enabled so the first call brings the daemon up.
type ipcClient struct{ opts Options }

func newIPCClient(opts Options) *ipcClient { return &ipcClient{opts: opts} }

func (c *ipcClient) roundtrip(req *ipc.Request) (*ipc.Response, error) {
	conn, err := ipc.Dial(c.opts.SocketPath, true, c.opts.SpawnArgs)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req.IPCVersion = ipc.IPCVersion
	if err := ipc.WriteReq(conn, req); err != nil {
		return nil, err
	}
	return ipc.ReadResp(conn)
}

func (c *ipcClient) Status() (*ipc.Status, error) {
	resp, err := c.roundtrip(&ipc.Request{Op: ipc.OpStatus})
	if err != nil {
		return nil, err
	}
	if resp.Kind == ipc.RespErr || resp.Status == nil {
		return nil, errors.New("status unavailable")
	}
	return resp.Status, nil
}

func (c *ipcClient) SendText(text string) (string, error) {
	resp, err := c.roundtrip(&ipc.Request{Op: ipc.OpSend, Force: "text", Bytes: []byte(text)})
	if err != nil {
		return "", err
	}
	if resp.Kind == ipc.RespErr {
		return "", errors.New(resp.Message)
	}
	if resp.Code == 4 { // SPEC §2: no peers is a warning, not a failure
		return resp.Message, nil
	}
	return "", nil
}

func (c *ipcClient) Copy(msgID, fallbackText string) error {
	req := &ipc.Request{Op: ipc.OpCopy, MsgID: msgID}
	if msgID == "" && fallbackText != "" {
		req.Bytes = []byte(fallbackText)
		req.Force = "text"
	}
	resp, err := c.roundtrip(req)
	if err != nil {
		return err
	}
	if resp.Kind == ipc.RespErr {
		return errors.New(resp.Message)
	}
	return nil
}

func (c *ipcClient) Clear(all bool) error {
	resp, err := c.roundtrip(&ipc.Request{Op: ipc.OpClear, All: all})
	if err != nil {
		return err
	}
	if resp.Kind == ipc.RespErr {
		return errors.New(resp.Message)
	}
	return nil
}

// streamItems holds a long-lived Subscribe connection and forwards each
// incoming item to the bubbletea program as an itemMsg. It reconnects on drop
// (which also auto-spawns the daemon) until ctx is cancelled, emitting
// connStateMsg so the header can reflect the live/lost state.
func streamItems(ctx context.Context, p *tea.Program, opts Options) {
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := ipc.Dial(opts.SocketPath, true, opts.SpawnArgs)
		if err != nil {
			p.Send(connStateMsg{err: err})
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		// Close the connection when the program exits so ReadResp unblocks.
		go func() { <-ctx.Done(); _ = conn.Close() }()

		if err := ipc.WriteReq(conn, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpSubscribe}); err != nil {
			_ = conn.Close()
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		p.Send(connStateMsg{subscribed: true})
		for {
			resp, err := ipc.ReadResp(conn)
			if err != nil {
				break
			}
			if resp.Kind == ipc.RespItem && resp.Item != nil {
				p.Send(itemMsg{item: resp.Item})
			}
		}
		_ = conn.Close()
		if ctx.Err() != nil {
			return
		}
		p.Send(connStateMsg{err: fmt.Errorf("subscription closed")})
		if !sleepCtx(ctx, time.Second) {
			return
		}
	}
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
