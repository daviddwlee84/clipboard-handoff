package cli

// Layer-1 TUI tests: drive the whole bubbletea model through a simulated
// terminal (charmbracelet/x/exp/teatest) and assert on the *rendered frames*,
// not just Update() state transitions (that layer lives in tui_test.go). These
// run in the default `go test ./...`; they are deterministic and finish in well
// under a second.
//
// Two teatest facts shape the assertions below:
//
//  1. Under `go test`, stdout is a pipe (not a TTY), so lipgloss falls back to
//     the Ascii color profile and renders *plain text* — no ANSI color/style
//     escapes wrap the content. That makes substring matching reliable and the
//     one golden frame stable.
//  2. tm.Output() is a *consuming* reader. teatest.WaitFor drains it into a
//     local buffer, so a second WaitFor only sees bytes written *after* the
//     first returned. Same-frame assertions are therefore grouped into a single
//     WaitFor(...containsAll); a later WaitFor only targets genuinely new output
//     (e.g. the quit prompt).

import (
	"bytes"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/ipc"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

// stableClient is the render-test daemon fake. It reuses fakeClient (tui_test.go)
// for Send/Copy/Paste/Clear and only pins Status() to a fixed, fully-populated
// snapshot so the 2s status tick can never drift the header mid-test (the base
// fakeClient.Status returns a partial snapshot).
type stableClient struct{ fakeClient }

func (*stableClient) Status() (*ipc.Status, error) {
	return &ipc.Status{Room: "default", Identity: "deadbeefcafefeed", AutoCopy: "notify", Clipboard: "available"}, nil
}

// newRenderModel builds the model exactly as cmdTUI would, with a stable daemon
// fake and a nil item stream (render tests inject items via tm.Send(itemMsg{})).
func newRenderModel() tuiModel {
	st := &ipc.Status{Room: "default", Identity: "deadbeefcafefeed", AutoCopy: "notify", Clipboard: "available"}
	return newTUIModel(&stableClient{}, "lan", st, nil)
}

// waitForAll blocks until every substring has appeared in the program's output
// (all read from the same accumulating buffer, so same-frame content matches in
// one call). Fails the test if the condition is not met in time.
func waitForAll(t *testing.T, tm *teatest.TestModel, subs ...string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		for _, s := range subs {
			if !bytes.Contains(b, []byte(s)) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(3*time.Second), teatest.WithCheckInterval(20*time.Millisecond))
}

// TestRenderIncomingItemAndHeader: an inbound item renders as a non-own chat
// bubble (sender + body), the header shows room/id/peers/auto_copy, and — since
// something was received this session — Ctrl-C raises the clear-on-quit prompt
// (SPEC §8) rather than exiting; declining ([n]) then exits cleanly.
func TestRenderIncomingItemAndHeader(t *testing.T) {
	tm := teatest.NewTestModel(t, newRenderModel(), teatest.WithInitialTermSize(80, 24))

	env := wire.Envelope{
		Type:       wire.TypeText,
		Text:       "hello from peer",
		DeviceName: "peerA",
		TS:         uint64(time.Now().UnixMilli()),
	}
	tm.Send(itemMsg{item: &ipc.Item{Envelope: env}})

	// Header fields (clipboard wraps off the line at 80 cols — asserted at a
	// wider size in TestRenderHeaderClipboard) and the incoming bubble, all in
	// one WaitFor because they share the accumulated output buffer.
	waitForAll(t, tm,
		"room default",     // header: room
		"id deadbeefca",    // header: id (shortID truncates the fingerprint)
		"peers 0",          // header: peers
		"auto_copy notify", // header: auto_copy mode
		"peerA",            // bubble: sender label (DeviceName)
		"hello from peer",  // bubble: body text
	)

	// Received-this-session => Ctrl-C prompts before quitting.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	waitForAll(t, tm, "clear this session before quitting")

	// Decline the clear -> program exits.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestRenderOwnMessageAndQuit: typing a line + Enter renders an own bubble
// (marked "(you)") with the typed text; nothing was *received*, so Ctrl-C quits
// immediately (no clear prompt).
func TestRenderOwnMessageAndQuit(t *testing.T) {
	tm := teatest.NewTestModel(t, newRenderModel(), teatest.WithInitialTermSize(80, 24))

	// Wait for the first frame so keystrokes land on a laid-out model.
	waitForAll(t, tm, "room default")

	tm.Type("hi there")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	waitForAll(t, tm, "you (you)", "hi there")

	// Nothing received this session -> Ctrl-C exits without the clear prompt.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestRenderHeaderClipboard: the full header — including the clipboard field,
// which wraps off an 80-col line — renders when the terminal is wide enough.
func TestRenderHeaderClipboard(t *testing.T) {
	tm := teatest.NewTestModel(t, newRenderModel(), teatest.WithInitialTermSize(120, 24))

	waitForAll(t, tm,
		"room default",
		"id deadbeefca",
		"peers 0",
		"auto_copy notify",
		"clipboard: available",
	)

	// No item received -> Ctrl-C quits straight away.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestRenderGolden pins one full 80x24 frame. It is deterministic because the
// Ascii color profile strips styling and the only clock-dependent field renders
// the relative time as "now" for a fresh message. Regenerate after intentional
// layout changes with:
//
//	go test ./cli/ -run TestRenderGolden -update
//
// Golden lives in cli/testdata/TestRenderGolden.golden. The captured surface is
// the *final model's* View() (not the streamed frame diff), so it is a single
// clean frame independent of renderer repaint timing.
func TestRenderGolden(t *testing.T) {
	tm := teatest.NewTestModel(t, newRenderModel(), teatest.WithInitialTermSize(80, 24))

	waitForAll(t, tm, "room default")
	tm.Type("golden frame")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitForAll(t, tm, "golden frame")

	// Browse mode blurs the composer (no blinking-cursor state) for a stable
	// frame; nothing was received, so Ctrl-C quits immediately.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})

	fm := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(tuiModel)
	teatest.RequireEqualOutput(t, []byte(fm.View()))
}
