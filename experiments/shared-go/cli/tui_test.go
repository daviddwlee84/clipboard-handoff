package cli

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/ipc"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

// fakeClient records outgoing IPC calls so the model can be exercised without a
// running daemon.
type fakeClient struct {
	sends  []fakeCall
	copies []fakeCall
	pastes int
	clears []bool // records Clear(all) calls
}

type fakeCall struct {
	force string
	data  []byte
}

func (f *fakeClient) Send(force string, data []byte) error {
	f.sends = append(f.sends, fakeCall{force, data})
	return nil
}
func (f *fakeClient) Copy(force string, data []byte) error {
	f.copies = append(f.copies, fakeCall{force, data})
	return nil
}
func (f *fakeClient) Paste() error         { f.pastes++; return nil }
func (f *fakeClient) Clear(all bool) error { f.clears = append(f.clears, all); return nil }
func (f *fakeClient) Status() (*ipc.Status, error) {
	return &ipc.Status{Room: "default", Clipboard: "available"}, nil
}

func newTestModel(cl tuiClient) tuiModel {
	st := &ipc.Status{Room: "default", Identity: "deadbeefcafefeed", AutoCopy: "notify", Clipboard: "available"}
	return newTUIModel(cl, "lan", st, nil) // nil stream: drive Update directly
}

// TestUpdateIncomingItemAppendsBubble: an inbound item from the daemon's event
// stream appends a (non-own) bubble.
func TestUpdateIncomingItemAppendsBubble(t *testing.T) {
	m := newTestModel(&fakeClient{})
	if len(m.bubbles) != 0 {
		t.Fatalf("expected empty history, got %d", len(m.bubbles))
	}

	env := wire.Envelope{
		Type:       wire.TypeText,
		Text:       "hello from peer",
		DeviceName: "peerA",
		Sender:     "peerAfingerprint",
		TS:         uint64(time.Now().UnixMilli()),
	}
	next, _ := m.Update(itemMsg{item: &ipc.Item{Envelope: env}})
	m = next.(tuiModel)

	if len(m.bubbles) != 1 {
		t.Fatalf("incoming item should append one bubble, got %d", len(m.bubbles))
	}
	b := m.bubbles[0]
	if b.own {
		t.Fatalf("incoming bubble must not be marked own")
	}
	if b.text != "hello from peer" || b.sender != "peerA" {
		t.Fatalf("bubble content mismatch: %+v", b)
	}
	if m.selected != 0 {
		t.Fatalf("selection should track the newest bubble, got %d", m.selected)
	}
}

// TestUpdateImageItemAppendsImageBubble: an inbound image becomes an image
// bubble carrying its metadata + local path.
func TestUpdateImageItemAppendsImageBubble(t *testing.T) {
	m := newTestModel(&fakeClient{})
	env := wire.Envelope{
		Type:       wire.TypeImage,
		DeviceName: "peerB",
		TS:         uint64(time.Now().UnixMilli()),
		Blob:       &wire.Blob{Hash: "0123456789abcdef", Size: 84 * 1024, W: 1440, H: 900},
	}
	next, _ := m.Update(itemMsg{item: &ipc.Item{Envelope: env, LocalPath: "/tmp/x.png"}})
	m = next.(tuiModel)

	if len(m.bubbles) != 1 || !m.bubbles[0].isImage {
		t.Fatalf("expected one image bubble, got %+v", m.bubbles)
	}
	if m.bubbles[0].path != "/tmp/x.png" || m.bubbles[0].w != 1440 || m.bubbles[0].h != 900 {
		t.Fatalf("image metadata mismatch: %+v", m.bubbles[0])
	}
}

// TestUpdateSubmitYieldsSend: typing text + Enter appends an own bubble and
// returns a command that performs the daemon Send.
func TestUpdateSubmitYieldsSend(t *testing.T) {
	fc := &fakeClient{}
	m := newTestModel(fc)
	m.composerFocused = true
	m.ta.SetValue("hi there")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)

	if len(m.bubbles) != 1 || !m.bubbles[0].own {
		t.Fatalf("submit should append one own bubble, got %+v", m.bubbles)
	}
	if m.bubbles[0].text != "hi there" {
		t.Fatalf("own bubble text mismatch: %q", m.bubbles[0].text)
	}
	if m.ta.Value() != "" {
		t.Fatalf("composer should reset after send, got %q", m.ta.Value())
	}
	if cmd == nil {
		t.Fatalf("submit should return a send command")
	}

	// Executing the returned command performs the Send and yields a sentMsg.
	msg := cmd()
	if _, ok := msg.(sentMsg); !ok {
		t.Fatalf("expected sentMsg, got %T", msg)
	}
	if len(fc.sends) != 1 || fc.sends[0].force != "text" || string(fc.sends[0].data) != "hi there" {
		t.Fatalf("Send not recorded correctly: %+v", fc.sends)
	}
}

// TestEmptySubmitNoSend: pressing Enter on empty composer neither adds a bubble
// nor sends.
func TestEmptySubmitNoSend(t *testing.T) {
	fc := &fakeClient{}
	m := newTestModel(fc)
	m.composerFocused = true
	m.ta.SetValue("   ")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)
	if len(m.bubbles) != 0 || cmd != nil {
		t.Fatalf("empty submit must be a no-op, got bubbles=%d cmd=%v", len(m.bubbles), cmd)
	}
	if len(fc.sends) != 0 {
		t.Fatalf("empty submit must not Send")
	}
}

// TestCopySelectedText: `y` in browse mode copies the highlighted text bubble
// to the clipboard via the daemon.
func TestCopySelectedText(t *testing.T) {
	fc := &fakeClient{}
	m := newTestModel(fc)
	env := wire.Envelope{Type: wire.TypeText, Text: "copy me", DeviceName: "peerA", TS: uint64(time.Now().UnixMilli())}
	next, _ := m.Update(itemMsg{item: &ipc.Item{Envelope: env}})
	m = next.(tuiModel)

	m.composerFocused = false // browse mode
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatalf("y should return a copy command")
	}
	if _, ok := cmd().(copiedMsg); !ok {
		t.Fatalf("expected copiedMsg")
	}
	if len(fc.copies) != 1 || fc.copies[0].force != "text" || string(fc.copies[0].data) != "copy me" {
		t.Fatalf("Copy not recorded correctly: %+v", fc.copies)
	}
}

// TestQuitPromptsClearAfterReceive: pressing q after receiving an item raises the
// clear prompt (does not quit); choosing [a] runs Clear(all=true) then quits.
func TestQuitPromptsClearAfterReceive(t *testing.T) {
	fc := &fakeClient{}
	m := newTestModel(fc)

	// Receive an item so the session has data.
	env := wire.Envelope{Type: wire.TypeText, Text: "hi", DeviceName: "peerA", TS: uint64(time.Now().UnixMilli())}
	next, _ := m.Update(itemMsg{item: &ipc.Item{Envelope: env}})
	m = next.(tuiModel)
	if !m.gotItem {
		t.Fatalf("gotItem should be set after receiving an item")
	}

	// q in browse mode should NOT quit yet; it opens the clear prompt.
	m.composerFocused = false
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = next.(tuiModel)
	if !m.confirmingQuit {
		t.Fatalf("q after receive should raise the clear prompt, not quit")
	}
	if cmd != nil {
		t.Fatalf("raising the prompt should not return a command (no quit yet)")
	}

	// Choosing [a] should run Clear(all=true) and then quit.
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if cmd == nil {
		t.Fatalf("choosing [a] should return the quit command")
	}
	if len(fc.clears) != 1 || fc.clears[0] != true {
		t.Fatalf("expected one Clear(all=true), got %+v", fc.clears)
	}
}

// TestQuitImmediateWhenNothingReceived: with no received items, q quits directly
// without the clear prompt.
func TestQuitImmediateWhenNothingReceived(t *testing.T) {
	m := newTestModel(&fakeClient{})
	m.composerFocused = false
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = next.(tuiModel)
	if m.confirmingQuit {
		t.Fatalf("q with nothing received should not raise the clear prompt")
	}
	if cmd == nil {
		t.Fatalf("q with nothing received should quit (non-nil command)")
	}
}
