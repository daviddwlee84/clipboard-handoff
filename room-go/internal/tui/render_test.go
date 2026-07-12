package tui

import (
	"bytes"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

// Layer 1 — rendered-output tests. Where tui_test.go asserts on the values
// model.Update returns, these run the model through teatest's simulated
// terminal (a real bubbletea program writing to an in-memory buffer) and assert
// on the *rendered frames*: the header chrome, a received chat bubble, an own
// (composed) bubble, and the clear-on-quit prompt.
//
// We deliberately prefer teatest.WaitFor + bytes.Contains over a golden
// RequireEqualOutput: the frames carry lipgloss SGR color codes and the whole
// screen is redrawn, so a byte-exact golden is flaky across terminfo/termenv
// and library versions. Substring assertions on the literal text are stable.
//
// Note on the 80-cell header: at width 80 the header line
//   room:… · 🔑fp · ● server · auto_copy:… · clipboard:…
// is longer than 80 cells and lipgloss truncates the tail, so `clipboard:…`
// gets cut off. We assert the surviving prefix at 80×24 (the size the brief
// asks for) and verify the clipboard field separately at a wider width below.

const (
	renderWait  = 5 * time.Second
	finalWait   = 5 * time.Second
	fpFull      = "SHA256:abcdefghijklmnop"
	fpShownHead = "🔑abcdefghijkl" // shortFP: 🔑 + first 12 chars of the body
)

func testStatus() *ipc.Status {
	return &ipc.Status{
		Room:        "demo",
		Fingerprint: fpFull,
		Server:      "room@localhost:2299",
		Connected:   true,
		AutoCopy:    "notify",
		Peers:       1,
		Clipboard:   true,
	}
}

func containsAll(b []byte, subs ...string) bool {
	for _, s := range subs {
		if !bytes.Contains(b, []byte(s)) {
			return false
		}
	}
	return true
}

// A received item renders as a chat bubble, and the header reflects the daemon
// status the model fetches on Init. Then Ctrl+C (with something received) shows
// the clear-on-quit prompt, which 'n' dismisses into a clean exit.
func TestRender_HeaderAndIncomingBubble(t *testing.T) {
	fc := &fakeClient{status: testStatus()}
	m := newModel(Options{Room: "demo"}, fc)

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))

	// Inject an incoming item on the same message type the Subscribe stream
	// delivers (see streamItems -> itemMsg in client.go).
	tm.Send(itemMsg{item: &ipc.Item{Envelope: wire.Envelope{
		Type:       wire.TypeText,
		Text:       "hello from a peer",
		Sender:     fpFull,
		DeviceName: "peerA",
		MsgID:      "m1",
	}}})

	// One combined WaitFor: the reader drains as it is read, so all substrings
	// asserted at a given model state must be checked in a single call.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return containsAll(b,
			// header (fingerprint + connected server only appear once the
			// Init status fetch has been applied — haveStatus == true):
			"room:demo",
			fpShownHead,
			"● room@localhost:2299",
			"auto_copy:notify",
			// the received bubble (sender label + body) and the notify-first nudge:
			"peerA",
			"hello from a peer",
			"press y to copy",
		)
	}, teatest.WithDuration(renderWait))

	// Ctrl+C after receiving surfaces the clear-on-quit prompt (SPEC §8) rather
	// than quitting outright.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("clear this session?"))
	}, teatest.WithDuration(renderWait))

	// 'n' = quit without clearing.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

// At a wider terminal the header's tail is not truncated, so the clipboard
// field renders in full.
func TestRender_HeaderClipboardField(t *testing.T) {
	fc := &fakeClient{status: testStatus()}
	m := newModel(Options{Room: "demo"}, fc)

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(120, 24))
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return containsAll(b, "room:demo", "auto_copy:notify", "clipboard:available")
	}, teatest.WithDuration(renderWait))

	// Nothing was received, so Ctrl+C quits immediately (no clear prompt).
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

// Typing into the composer and pressing Enter renders the user's own bubble
// ("you") and drives a text send to the daemon.
func TestRender_ComposeAndSend(t *testing.T) {
	fc := &fakeClient{status: testStatus()}
	m := newModel(Options{Room: "demo"}, fc)
	// Focus the composer, exactly as entering compose mode does (onKeyBrowse
	// "i"/Enter calls m.ta.Focus()). The bubbles textarea ignores key input until
	// it is focused, so without this the typed runes would be dropped.
	m.ta.Focus()

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))

	tm.Type("ship it")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return containsAll(b, "you", "ship it")
	}, teatest.WithDuration(renderWait))

	// The send command runs in a background goroutine; poll the (mutex-guarded)
	// recorder until it lands, so we assert the send fired without racing it.
	if !eventually(2*time.Second, func() bool {
		s := fc.sends()
		return len(s) == 1 && s[0] == "ship it"
	}) {
		t.Fatalf("daemon SendText not recorded, got %v", fc.sends())
	}

	// Own sends are not "received", so Ctrl+C quits immediately.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
