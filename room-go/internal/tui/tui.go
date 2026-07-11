// Package tui implements `room tui` (SPEC §4): a messenger-style chat attached
// to the local client daemon. It Subscribes to the daemon's IPC event stream
// (the same stream `recv --follow` uses), renders incoming items as chat
// bubbles in a bubbles/viewport, sends typed text via the daemon, and drives
// the daemon's clipboard for the `y` copy action — the TUI is a front-end, not
// a second clipboard owner (SPEC §4 notify-first).
package tui

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

// bubble is one rendered chat message.
type bubble struct {
	item     ipc.Item
	mine     bool // originated from this device (shown right-aligned)
	accepted bool // notify mode: user has copied it (clears the "press y" nudge)
}

// ---- bubbletea messages ----

type itemMsg struct{ item *ipc.Item }
type statusMsg struct {
	st  *ipc.Status
	err error
}
type sendResultMsg struct {
	warn string
	err  error
}
type actionResultMsg struct {
	msg string
	err error
}
type connStateMsg struct {
	subscribed bool
	err        error
}
type tickMsg time.Time

type model struct {
	client daemonClient
	opts   Options

	width, height int

	vp viewport.Model
	ta textarea.Model

	bubbles  []bubble
	selected int  // index into bubbles; -1 == none highlighted (y uses latest)
	focused  bool // composer (textarea) focused vs. browse mode

	status     ipc.Status
	haveStatus bool
	live       bool // subscription is currently connected

	toast      string
	toastUntil time.Time

	receivedCount int  // received (non-mine) items this TUI session (SPEC §8 quit)
	quitting      bool // showing the clear-on-quit prompt

	lineStarts   []int // vp-content start line of each bubble (for scroll-to-selection)
	contentLines int   // total lines in the current vp content
}

func newModel(opts Options, client daemonClient) model {
	ta := textarea.New()
	ta.Placeholder = "Type a message… (Enter to send, Esc to browse)"
	ta.Prompt = "┃ "
	ta.CharLimit = 8000
	ta.ShowLineNumbers = false
	ta.SetHeight(3)

	m := model{
		client:   client,
		opts:     opts,
		vp:       viewport.New(80, 20),
		ta:       ta,
		selected: -1,
		focused:  true,
	}
	m.resize(80, 24)
	return m
}

// Run launches the TUI: it starts the Subscribe streamer, then runs the
// bubbletea program on the alt screen until the user quits.
func Run(opts Options) error {
	m := newModel(opts, newIPCClient(opts))
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go streamItems(ctx, p, opts)

	_, err := p.Run()
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.fetchStatusCmd(), tickCmd())
}

// ---- commands ----

func (m model) fetchStatusCmd() tea.Cmd {
	return func() tea.Msg {
		st, err := m.client.Status()
		return statusMsg{st: st, err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) sendTextCmd(text string) tea.Cmd {
	return func() tea.Msg {
		warn, err := m.client.SendText(text)
		return sendResultMsg{warn: warn, err: err}
	}
}

func (m model) copyCmd(msgID, fallback, label string) tea.Cmd {
	return func() tea.Msg {
		if err := m.client.Copy(msgID, fallback); err != nil {
			return actionResultMsg{err: err}
		}
		return actionResultMsg{msg: "copied " + label + " to clipboard"}
	}
}

// saveImageCmd copies a materialized image blob to a user-visible location.
func saveImageCmd(b bubble) tea.Cmd {
	return func() tea.Msg {
		src := b.item.LocalPath
		if src == "" {
			return actionResultMsg{err: errNoLocalImage}
		}
		dst, err := saveImage(src, b.item.Envelope.Blob)
		if err != nil {
			return actionResultMsg{err: err}
		}
		return actionResultMsg{msg: "saved image → " + dst}
	}
}

// openImageCmd opens an image externally (macOS `open`, Linux `xdg-open`, …).
func openImageCmd(b bubble) tea.Cmd {
	return func() tea.Msg {
		src := b.item.LocalPath
		if src == "" {
			return actionResultMsg{err: errNoLocalImage}
		}
		if err := openExternally(src); err != nil {
			return actionResultMsg{err: err}
		}
		return actionResultMsg{msg: "opened " + filepath.Base(src)}
	}
}

// ---- update ----

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		m.rebuild(true)
		return m, nil

	case itemMsg:
		return m.onItem(msg.item), nil

	case statusMsg:
		if msg.err == nil && msg.st != nil {
			m.status = *msg.st
			m.haveStatus = true
		}
		return m, nil

	case connStateMsg:
		m.live = msg.subscribed
		return m, nil

	case sendResultMsg:
		if msg.err != nil {
			m.setToast("send failed: " + msg.err.Error())
		} else if msg.warn != "" {
			m.setToast("sent (warning: " + msg.warn + ")")
		}
		return m, nil

	case actionResultMsg:
		if msg.err != nil {
			m.setToast("error: " + msg.err.Error())
		} else if msg.msg != "" {
			m.setToast(msg.msg)
		}
		return m, nil

	case tickMsg:
		if m.toast != "" && time.Now().After(m.toastUntil) {
			m.toast = ""
		}
		return m, tea.Batch(m.fetchStatusCmd(), tickCmd())

	case tea.KeyMsg:
		return m.onKey(msg)

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}

	// Forward anything else to the focused component.
	var cmd tea.Cmd
	if m.focused {
		m.ta, cmd = m.ta.Update(msg)
	} else {
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

// onItem appends a received bubble and updates selection/scroll.
func (m model) onItem(item *ipc.Item) model {
	if item == nil {
		return m
	}
	m.receivedCount++
	atBottom := m.vp.AtBottom()
	accepted := m.status.AutoCopy != "notify" // notify mode needs an explicit copy
	m.bubbles = append(m.bubbles, bubble{item: *item, mine: false, accepted: accepted})
	m.rebuild(atBottom)
	if !accepted {
		label := item.Envelope.DeviceName
		if label == "" {
			label = shortID(item.Envelope.Sender)
		}
		m.setToast("new item from " + label + " — press y to copy")
	}
	return m
}

func (m model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The clear-on-quit prompt (SPEC §8) captures keys until resolved.
	if m.quitting {
		return m.onKeyQuitPrompt(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		return m.maybeQuit()
	}

	if m.focused {
		return m.onKeyCompose(msg)
	}
	return m.onKeyBrowse(msg)
}

// maybeQuit shows the clear-on-quit prompt when something was received this
// session (SPEC §8), else quits immediately.
func (m model) maybeQuit() (tea.Model, tea.Cmd) {
	if m.anythingReceived() {
		m.quitting = true
		m.rebuild(false)
		return m, nil
	}
	return m, tea.Quit
}

// anythingReceived reports whether this session received any item — either
// observed live in the TUI, or already sitting in the daemon buffer.
func (m model) anythingReceived() bool {
	return m.receivedCount > 0 || m.status.Buffer > 0
}

// onKeyQuitPrompt handles the "clear this session?" choice before exiting. The
// clear runs on the daemon (SPEC §8) via the same logic as `room clear`.
func (m model) onKeyQuitPrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "t":
		return m, tea.Sequence(m.clearCmd(false), tea.Quit)
	case "a":
		return m, tea.Sequence(m.clearCmd(true), tea.Quit)
	case "n", "esc", "q":
		return m, tea.Quit
	case "ctrl+c":
		return m, tea.Quit // force quit without clearing
	}
	return m, nil // ignore other keys while the prompt is up
}

// clearCmd asks the daemon to clear this session (transient, or all incl sinks).
func (m model) clearCmd(all bool) tea.Cmd {
	return func() tea.Msg {
		_ = m.client.Clear(all)
		return nil
	}
}

// onKeyCompose handles keys while the composer is focused.
func (m model) onKeyCompose(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.focused = false
		m.ta.Blur()
		if m.selected < 0 && len(m.bubbles) > 0 {
			m.selected = len(m.bubbles) - 1
		}
		m.rebuild(false)
		return m, nil
	case "enter":
		text := strings.TrimRight(m.ta.Value(), "\n")
		if strings.TrimSpace(text) == "" {
			return m, nil
		}
		// Optimistic own bubble: the daemon does not echo our own sends back on
		// the Subscribe stream (the broker excludes the sender), so we append it
		// locally.
		env := wire.Envelope{
			V: wire.Version, Type: wire.TypeText, Mime: wire.MimeText,
			Text: text, Sender: "you", DeviceName: "you",
			TS: uint64(time.Now().UnixMilli()),
		}
		m.bubbles = append(m.bubbles, bubble{item: ipc.Item{Envelope: env}, mine: true, accepted: true})
		m.ta.Reset()
		m.rebuild(true)
		return m, m.sendTextCmd(text)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// onKeyBrowse handles keys in browse mode (composer blurred).
func (m model) onKeyBrowse(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m.maybeQuit()
	case "i", "a", "enter":
		m.focused = true
		m.rebuild(false)
		return m, m.ta.Focus()
	case "esc":
		m.focused = true
		m.rebuild(false)
		return m, m.ta.Focus()
	case "up", "k":
		m.moveSelection(-1)
		return m, nil
	case "down", "j":
		m.moveSelection(1)
		return m, nil
	case "g", "home":
		if len(m.bubbles) > 0 {
			m.selected = 0
		}
		m.rebuild(false)
		m.scrollToSelection()
		return m, nil
	case "G", "end":
		if len(m.bubbles) > 0 {
			m.selected = len(m.bubbles) - 1
		}
		m.rebuild(false)
		m.scrollToSelection()
		return m, nil
	case "y":
		return m, m.copySelected()
	case "s":
		if b, ok := m.selectedImage(); ok {
			return m, saveImageCmd(b)
		}
		m.setToast("no image selected to save")
		return m, nil
	case "o":
		if b, ok := m.selectedImage(); ok {
			return m, openImageCmd(b)
		}
		m.setToast("no image selected to open")
		return m, nil
	}
	// Fall through: let the viewport scroll (pgup/pgdn/space/etc.).
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// copySelected asks the daemon to copy the highlighted (or latest) item.
func (m *model) copySelected() tea.Cmd {
	idx := m.effectiveSelection()
	if idx < 0 {
		m.setToast("nothing to copy")
		return nil
	}
	b := m.bubbles[idx]
	b.accepted = true
	m.bubbles[idx] = b
	m.rebuild(false)
	env := b.item.Envelope
	label := "message"
	if env.Type == wire.TypeImage {
		label = "image"
	}
	if b.mine {
		// Our own sent item is not in the daemon buffer; copy its text inline.
		return m.copyCmd("", env.Text, label)
	}
	return m.copyCmd(env.MsgID, "", label)
}

func (m *model) moveSelection(delta int) {
	if len(m.bubbles) == 0 {
		return
	}
	if m.selected < 0 {
		m.selected = len(m.bubbles) - 1
	} else {
		m.selected += delta
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected > len(m.bubbles)-1 {
		m.selected = len(m.bubbles) - 1
	}
	m.rebuild(false)
	m.scrollToSelection()
}

// effectiveSelection returns the highlighted index, or the latest bubble when
// nothing is explicitly selected.
func (m model) effectiveSelection() int {
	if len(m.bubbles) == 0 {
		return -1
	}
	if m.selected < 0 || m.selected >= len(m.bubbles) {
		return len(m.bubbles) - 1
	}
	return m.selected
}

func (m model) selectedImage() (bubble, bool) {
	idx := m.effectiveSelection()
	if idx < 0 {
		return bubble{}, false
	}
	b := m.bubbles[idx]
	if b.item.Envelope.Type != wire.TypeImage {
		return bubble{}, false
	}
	return b, true
}

func (m *model) setToast(s string) {
	m.toast = s
	m.toastUntil = time.Now().Add(5 * time.Second)
}

// ---- external image actions ----

func openExternally(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}
