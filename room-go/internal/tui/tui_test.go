package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

// fakeClient records daemon calls so Update can be exercised without a live
// daemon / socket.
type fakeClient struct {
	sent   []string
	copies []copyCall
	clears []bool // Clear(all) calls, in order
	status *ipc.Status
}

type copyCall struct{ msgID, fallback string }

func (f *fakeClient) Status() (*ipc.Status, error)      { return f.status, nil }
func (f *fakeClient) SendText(t string) (string, error) { f.sent = append(f.sent, t); return "", nil }
func (f *fakeClient) Copy(msgID, fallback string) error {
	f.copies = append(f.copies, copyCall{msgID, fallback})
	return nil
}
func (f *fakeClient) Clear(all bool) error { f.clears = append(f.clears, all); return nil }

func asModel(t *testing.T, tm tea.Model) model {
	t.Helper()
	m, ok := tm.(model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.model", tm)
	}
	return m
}

// An incoming item message appends exactly one (non-mine) bubble.
func TestUpdate_IncomingItemAppendsBubble(t *testing.T) {
	m := newModel(Options{}, &fakeClient{})
	item := &ipc.Item{Envelope: wire.Envelope{
		Type:       wire.TypeText,
		Text:       "hello from a peer",
		Sender:     "SHA256:abcdef0123456789",
		DeviceName: "peerA",
		MsgID:      "msg-1",
	}}

	out, _ := m.Update(itemMsg{item: item})
	got := asModel(t, out)

	if len(got.bubbles) != 1 {
		t.Fatalf("bubbles = %d, want 1", len(got.bubbles))
	}
	b := got.bubbles[0]
	if b.mine {
		t.Errorf("incoming bubble marked as mine")
	}
	if b.item.Envelope.Text != "hello from a peer" {
		t.Errorf("bubble text = %q", b.item.Envelope.Text)
	}
}

// Submitting the composer appends the user's own bubble and yields a send to
// the daemon carrying the typed text.
func TestUpdate_ComposerSubmitSends(t *testing.T) {
	fc := &fakeClient{}
	m := newModel(Options{}, fc)
	m.focused = true
	m.ta.SetValue("ship it")

	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := asModel(t, out)

	if len(got.bubbles) != 1 || !got.bubbles[0].mine {
		t.Fatalf("expected one own bubble, got %+v", got.bubbles)
	}
	if got.bubbles[0].item.Envelope.Text != "ship it" {
		t.Errorf("own bubble text = %q", got.bubbles[0].item.Envelope.Text)
	}
	if got.ta.Value() != "" {
		t.Errorf("composer not reset, still %q", got.ta.Value())
	}
	if cmd == nil {
		t.Fatal("expected a send command, got nil")
	}
	// Run the returned command; it must produce a sendResultMsg and record the
	// send on the daemon client.
	if _, ok := cmd().(sendResultMsg); !ok {
		t.Errorf("send command did not yield sendResultMsg")
	}
	if len(fc.sent) != 1 || fc.sent[0] != "ship it" {
		t.Errorf("daemon sent = %v, want [ship it]", fc.sent)
	}
}

// Empty/whitespace composer submits are ignored (no bubble, no send).
func TestUpdate_EmptySubmitIgnored(t *testing.T) {
	fc := &fakeClient{}
	m := newModel(Options{}, fc)
	m.focused = true
	m.ta.SetValue("   ")

	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := asModel(t, out)
	if len(got.bubbles) != 0 {
		t.Errorf("bubbles = %d, want 0", len(got.bubbles))
	}
	if len(fc.sent) != 0 {
		t.Errorf("unexpected send: %v", fc.sent)
	}
}

// In browse mode, `y` copies the highlighted received item by msg_id via the
// daemon (the daemon is the clipboard owner).
func TestUpdate_CopyByMsgID(t *testing.T) {
	fc := &fakeClient{}
	m := newModel(Options{}, fc)
	// Ingest one received item, then enter browse mode and copy it.
	out, _ := m.Update(itemMsg{item: &ipc.Item{Envelope: wire.Envelope{
		Type: wire.TypeText, Text: "grab me", MsgID: "msg-42", DeviceName: "peerB",
	}}})
	m = asModel(t, out)
	m.focused = false
	m.selected = 0

	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	_ = asModel(t, out)
	if cmd == nil {
		t.Fatal("expected a copy command")
	}
	cmd() // executes the copy
	if len(fc.copies) != 1 || fc.copies[0].msgID != "msg-42" {
		t.Errorf("copies = %+v, want one copy of msg-42", fc.copies)
	}
}

func runeKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// Quitting with nothing received exits immediately (no clear prompt).
func TestQuit_ImmediateWhenNothingReceived(t *testing.T) {
	m := newModel(Options{}, &fakeClient{})
	m.focused = false // browse mode so `q` means quit

	out, cmd := m.Update(runeKey('q'))
	got := asModel(t, out)
	if got.quitting {
		t.Fatalf("should not prompt when nothing was received")
	}
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", cmd())
	}
}

// Quitting after receiving an item shows the clear prompt instead of quitting.
func TestQuit_PromptsWhenReceived(t *testing.T) {
	m := newModel(Options{}, &fakeClient{})
	out, _ := m.Update(itemMsg{item: &ipc.Item{Envelope: wire.Envelope{Type: wire.TypeText, Text: "hi", MsgID: "m1"}}})
	m = asModel(t, out)
	m.focused = false

	out, cmd := m.Update(runeKey('q'))
	got := asModel(t, out)
	if !got.quitting {
		t.Fatalf("expected the clear-on-quit prompt to appear")
	}
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatalf("should not quit yet — the prompt must be answered first")
		}
	}
}

// Answering the prompt with 'n' quits without clearing.
func TestQuit_PromptDeclineDoesNotClear(t *testing.T) {
	fc := &fakeClient{}
	m := newModel(Options{}, fc)
	m.quitting = true

	out, cmd := m.Update(runeKey('n'))
	_ = asModel(t, out)
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", cmd())
	}
	if len(fc.clears) != 0 {
		t.Fatalf("declining should not clear, got clears=%v", fc.clears)
	}
}

// clearCmd(true/false) asks the daemon to clear the session (all vs transient).
func TestQuit_ClearCmdCallsDaemon(t *testing.T) {
	fc := &fakeClient{}
	m := newModel(Options{}, fc)

	m.clearCmd(true)()
	m.clearCmd(false)()
	if len(fc.clears) != 2 || fc.clears[0] != true || fc.clears[1] != false {
		t.Fatalf("clears = %v, want [true false]", fc.clears)
	}
}
