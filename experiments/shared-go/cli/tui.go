// This file implements `BIN tui` (SPEC §4): a lean, messenger-style chat that
// attaches to the local daemon over the same IPC surface the other clients use.
// It reuses the daemon's Subscribe event stream (as `recv --follow` does) for
// live incoming items and the Send/Copy/Paste/Status ops for actions — it does
// NOT touch the transport, discovery, ring buffer or clipboard directly (the
// daemon owns all of that). Because it lives in shared-go/cli, both probes
// (`lan`, `libp2p-mesh`) get an identical TUI for free.
//
// Layout (top → bottom): header (room · id · peers · auto_copy · clipboard) ·
// scrollable bubble history (sender · relative-time · body; own messages
// marked) · composer (textarea) · keybinding hint line.
package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/ipc"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

// ---- IPC client used by the TUI --------------------------------------------

// tuiClient is the small daemon surface the model needs. It is an interface so
// the model can be unit-tested with a fake (no running daemon).
type tuiClient interface {
	Send(force string, data []byte) error // broadcast an item (OpSend)
	Copy(force string, data []byte) error // write bytes to the OS clipboard (OpCopy)
	Paste() error                         // copy the daemon's latest item (OpPaste)
	Status() (*ipc.Status, error)         // header snapshot (OpStatus)
}

// ipcClient is the real tuiClient: each call is one short IPC round-trip on a
// fresh connection, auto-spawning the daemon if needed (like every other CLI
// command).
type ipcClient struct {
	socket string
	spawn  []string
}

func (c *ipcClient) do(req *ipc.Request) (*ipc.Response, error) {
	conn, err := ipc.Dial(c.socket, true, c.spawn)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := ipc.WriteReq(conn, req); err != nil {
		return nil, err
	}
	return ipc.ReadResp(conn)
}

func (c *ipcClient) simple(req *ipc.Request) error {
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.Kind == ipc.RespErr {
		return errors.New(resp.Message)
	}
	return nil
}

func (c *ipcClient) Send(force string, data []byte) error {
	// A "no peers yet" reply is RespOK code 4 (a warning) — treated as success.
	return c.simple(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpSend, Bytes: data, Force: force})
}

func (c *ipcClient) Copy(force string, data []byte) error {
	return c.simple(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpCopy, Bytes: data, Force: force})
}

func (c *ipcClient) Paste() error {
	return c.simple(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpPaste})
}

func (c *ipcClient) Status() (*ipc.Status, error) {
	resp, err := c.do(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpStatus})
	if err != nil {
		return nil, err
	}
	if resp.Kind == ipc.RespErr || resp.Status == nil {
		return nil, errors.New("status unavailable")
	}
	return resp.Status, nil
}

// ---- cmdTUI: wire the daemon to the bubbletea program ----------------------

func (g Globals) cmdTUI(args []string) int {
	_ = args // no TUI-specific flags in the lean version
	sock, err := g.socketPath()
	if err != nil {
		return g.fail(1, err)
	}
	cl := &ipcClient{socket: sock, spawn: g.spawnArgs()}

	// Initial header snapshot; this also auto-spawns the daemon if it is down.
	st, err := cl.Status()
	if err != nil {
		return g.fail(3, err)
	}

	// A dedicated long-lived connection carries the live event stream (same op
	// `recv --follow` uses). A goroutine pumps items into a channel the model
	// drains via a bubbletea command.
	subConn, err := ipc.Dial(sock, true, g.spawnArgs())
	if err != nil {
		return g.fail(3, err)
	}
	if err := ipc.WriteReq(subConn, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpSubscribe}); err != nil {
		subConn.Close()
		return g.fail(3, err)
	}
	items := make(chan *ipc.Item, 64)
	go func() {
		defer close(items)
		for {
			resp, rerr := ipc.ReadResp(subConn)
			if rerr != nil {
				return
			}
			if resp.Kind == ipc.RespItem && resp.Item != nil {
				items <- resp.Item
			}
		}
	}()

	m := newTUIModel(cl, g.app.BinName, st, items)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, rerr := p.Run()
	subConn.Close() // unblocks the pump goroutine
	if rerr != nil {
		return g.fail(1, rerr)
	}
	return 0
}

// ---- model -----------------------------------------------------------------

// bubble is one rendered chat message.
type bubble struct {
	own    bool
	sender string
	ts     time.Time

	isImage  bool
	text     string
	w, h     int
	size     uint64
	path     string // daemon-materialized PNG (received images only)
	filename string
}

type tuiModel struct {
	client  tuiClient
	binName string
	items   <-chan *ipc.Item // nil in unit tests

	// header
	room, self, autoCopy, clipboard string
	peers                           int

	bubbles         []bubble
	selected        int // index into bubbles; -1 when empty
	composerFocused bool

	ta     textarea.Model
	vp     viewport.Model
	width  int
	height int

	toast string
}

func newTUIModel(cl tuiClient, binName string, st *ipc.Status, items <-chan *ipc.Item) tuiModel {
	ta := textarea.New()
	ta.Placeholder = "Type a message… (Enter sends, Esc browses)"
	ta.Prompt = "┃ "
	ta.ShowLineNumbers = false
	ta.SetHeight(2)
	ta.Focus()

	m := tuiModel{
		client:          cl,
		binName:         binName,
		items:           items,
		ta:              ta,
		vp:              viewport.New(80, 20),
		composerFocused: true,
		selected:        -1,
		width:           80,
		height:          24,
	}
	m.applyStatus(st)
	m.layout()
	m.refreshViewport(true)
	return m
}

func (m *tuiModel) applyStatus(st *ipc.Status) {
	if st == nil {
		return
	}
	m.room = st.Room
	m.self = shortID(st.Identity)
	m.autoCopy = st.AutoCopy
	m.peers = len(st.Peers)
	if st.Clipboard != "" {
		m.clipboard = st.Clipboard
	} else if m.clipboard == "" {
		m.clipboard = "unknown"
	}
}

func (m tuiModel) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, tickStatus()}
	if m.items != nil {
		cmds = append(cmds, waitForItem(m.items))
	}
	return tea.Batch(cmds...)
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.refreshViewport(false)
		return m, nil

	case itemMsg:
		m.appendItem(msg.item)
		m.refreshViewport(true)
		if m.items != nil {
			return m, waitForItem(m.items)
		}
		return m, nil

	case streamClosedMsg:
		m.toast = "event stream closed (daemon gone?)"
		return m, nil

	case sentMsg:
		if msg.err != nil {
			m.toast = "send failed: " + msg.err.Error()
		}
		return m, nil

	case copiedMsg:
		if msg.err != nil {
			m.toast = "copy failed: " + msg.err.Error()
		} else if msg.label != "" {
			m.toast = msg.label
		}
		return m, nil

	case savedMsg:
		if msg.err != nil {
			m.toast = "save failed: " + msg.err.Error()
		} else {
			m.toast = "saved → " + msg.path
		}
		return m, nil

	case openedMsg:
		if msg.err != nil {
			m.toast = "open failed: " + msg.err.Error()
		} else {
			m.toast = "opened externally"
		}
		return m, nil

	case statusTickMsg:
		return m, tea.Batch(fetchStatus(m.client), tickStatus())

	case statusMsg:
		m.applyStatus(msg.st)
		return m, nil
	}

	// Route everything else (cursor blink, etc.) to the focused component.
	var cmd tea.Cmd
	if m.composerFocused {
		m.ta, cmd = m.ta.Update(msg)
	} else {
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func (m tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.toggleFocus()
		return m, nil
	}

	if m.composerFocused {
		if msg.String() == "enter" {
			text := strings.TrimSpace(m.ta.Value())
			if text == "" {
				return m, nil
			}
			m.appendOwnText(text)
			m.ta.Reset()
			m.refreshViewport(true)
			return m, sendText(m.client, text)
		}
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	}

	// Browse mode: navigate + per-item actions.
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.moveSelection(-1)
		return m, nil
	case "down", "j":
		m.moveSelection(1)
		return m, nil
	case "g", "home":
		if len(m.bubbles) > 0 {
			m.selected = 0
			m.refreshViewport(false)
		}
		return m, nil
	case "G", "end":
		if len(m.bubbles) > 0 {
			m.selected = len(m.bubbles) - 1
			m.refreshViewport(false)
		}
		return m, nil
	case "y":
		return m, m.copySelected()
	case "s":
		return m, m.saveSelected()
	case "o":
		return m, m.openSelected()
	case "p":
		return m, pasteLatest(m.client)
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *tuiModel) toggleFocus() {
	m.composerFocused = !m.composerFocused
	if m.composerFocused {
		m.ta.Focus()
	} else {
		m.ta.Blur()
		if m.selected < 0 && len(m.bubbles) > 0 {
			m.selected = len(m.bubbles) - 1
		}
	}
	m.refreshViewport(false)
}

func (m *tuiModel) moveSelection(delta int) {
	if len(m.bubbles) == 0 {
		return
	}
	if m.selected < 0 {
		m.selected = len(m.bubbles) - 1
	}
	m.selected += delta
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(m.bubbles) {
		m.selected = len(m.bubbles) - 1
	}
	m.refreshViewport(false)
}

func (m *tuiModel) appendOwnText(text string) {
	wasLast := m.selected == len(m.bubbles)-1
	m.bubbles = append(m.bubbles, bubble{own: true, sender: "you", ts: time.Now(), text: text})
	if wasLast || m.selected < 0 {
		m.selected = len(m.bubbles) - 1
	}
}

func (m *tuiModel) appendItem(it *ipc.Item) {
	env := &it.Envelope
	wasLast := m.selected == len(m.bubbles)-1
	b := bubble{sender: senderLabel(env), ts: time.UnixMilli(int64(env.TS))}
	if env.Type == wire.TypeImage && env.Blob != nil {
		b.isImage = true
		b.w, b.h = int(env.Blob.W), int(env.Blob.H)
		b.size = env.Blob.Size
		b.path = it.LocalPath
		b.filename = shortHash(env.Blob.Hash) + ".png"
	} else {
		b.text = env.Text
	}
	m.bubbles = append(m.bubbles, b)
	if wasLast || m.selected < 0 {
		m.selected = len(m.bubbles) - 1
	}
}

func (m tuiModel) currentBubble() (bubble, bool) {
	if len(m.bubbles) == 0 {
		return bubble{}, false
	}
	i := m.selected
	if i < 0 || i >= len(m.bubbles) {
		i = len(m.bubbles) - 1
	}
	return m.bubbles[i], true
}

// ---- actions (each returns a tea.Cmd so IPC/file work is off the UI) --------

func (m tuiModel) copySelected() tea.Cmd {
	b, ok := m.currentBubble()
	if !ok {
		return nil
	}
	if b.isImage {
		return copyImage(m.client, b.path)
	}
	return copyText(m.client, b.text)
}

func (m tuiModel) saveSelected() tea.Cmd {
	b, ok := m.currentBubble()
	if !ok || !b.isImage {
		return func() tea.Msg { return savedMsg{err: errors.New("selected item is not an image")} }
	}
	return saveImage(b.path, b.filename)
}

func (m tuiModel) openSelected() tea.Cmd {
	b, ok := m.currentBubble()
	if !ok || !b.isImage {
		return func() tea.Msg { return openedMsg{err: errors.New("selected item is not an image")} }
	}
	return openExternal(b.path)
}

// ---- rendering -------------------------------------------------------------

var (
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62")).Padding(0, 1).MaxHeight(1)
	hintStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Padding(0, 1).MaxHeight(1)
	toastStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	senderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	ownStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	bubbleStyle   = lipgloss.NewStyle().Padding(0, 1).Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color("237"))
	selectedStyle = lipgloss.NewStyle().Padding(0, 1).Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("205"))
	labelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
)

func (m *tuiModel) layout() {
	if m.width <= 0 {
		m.width = 80
	}
	if m.height <= 0 {
		m.height = 24
	}
	m.ta.SetWidth(m.width - 2)
	// header 1 + hint 1 + composer 3 (label + 2 rows) = 5 reserved rows.
	vpH := m.height - 5
	if vpH < 3 {
		vpH = 3
	}
	m.vp.Width = m.width
	m.vp.Height = vpH
}

// refreshViewport rebuilds the history content and scrolls so the selected
// bubble stays visible; follow (or a selection at the end) pins to the bottom.
func (m *tuiModel) refreshViewport(follow bool) {
	content, selStart, selHeight := m.renderHistory()
	m.vp.SetContent(content)
	if follow || m.selected < 0 || m.selected == len(m.bubbles)-1 {
		m.vp.GotoBottom()
		return
	}
	top := m.vp.YOffset
	bottom := top + m.vp.Height
	switch {
	case selStart < top:
		m.vp.SetYOffset(selStart)
	case selStart+selHeight > bottom:
		m.vp.SetYOffset(selStart + selHeight - m.vp.Height)
	}
}

func (m *tuiModel) renderHistory() (content string, selStart, selHeight int) {
	if len(m.bubbles) == 0 {
		return dimStyle.Render("No messages yet — type below and press Enter to send."), 0, 0
	}
	blocks := make([]string, 0, len(m.bubbles))
	line := 0
	for i, b := range m.bubbles {
		block := m.renderBubble(b, i == m.selected)
		h := lipgloss.Height(block)
		if i == m.selected {
			selStart, selHeight = line, h
		}
		blocks = append(blocks, block)
		line += h
	}
	return strings.Join(blocks, "\n"), selStart, selHeight
}

func (m *tuiModel) renderBubble(b bubble, selected bool) string {
	hdr := senderStyle
	if b.own {
		hdr = ownStyle
	}
	marker := ""
	if b.own {
		marker = " (you)"
	}
	head := hdr.Render(b.sender+marker) + dimStyle.Render(" · "+relTime(b.ts))

	var body string
	if b.isImage {
		body = fmt.Sprintf("🖼  %s  %d×%d · %s", b.filename, b.w, b.h, humanSize(b.size))
		body += "\n" + dimStyle.Render("[y] copy · [s] save · [o] open   (inline preview: stubbed)")
	} else {
		body = wrap(b.text, m.vp.Width-4)
	}

	style := bubbleStyle
	if selected {
		style = selectedStyle
	}
	return style.Render(head + "\n" + body)
}

func (m tuiModel) View() string {
	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(),
		m.vp.View(),
		m.composerView(),
		m.renderHint(),
	)
}

func (m tuiModel) renderHeader() string {
	parts := []string{
		"room " + m.room,
		"id " + m.self,
		fmt.Sprintf("peers %d", m.peers),
		"auto_copy " + m.autoCopy,
		"clipboard: " + m.clipboard,
	}
	return headerStyle.Width(m.width).Render(m.binName + " tui   " + strings.Join(parts, " · "))
}

func (m tuiModel) composerView() string {
	label := "› message"
	if !m.composerFocused {
		label = "browsing — Esc to compose"
	}
	return labelStyle.Render(label) + "\n" + m.ta.View()
}

func (m tuiModel) renderHint() string {
	var h string
	if m.composerFocused {
		h = "Enter send · Esc browse · Ctrl-C quit"
	} else {
		h = "↑/↓ select · y copy · s save · o open · p paste-latest · Esc compose · q quit"
	}
	if m.toast != "" {
		h = toastStyle.Render(m.toast) + dimStyle.Render("  —  "+h)
	}
	return hintStyle.Width(m.width).Render(h)
}

// ---- messages + commands ---------------------------------------------------

type (
	itemMsg         struct{ item *ipc.Item }
	streamClosedMsg struct{}
	sentMsg         struct{ err error }
	copiedMsg       struct {
		err   error
		label string
	}
	savedMsg struct {
		err  error
		path string
	}
	openedMsg     struct{ err error }
	statusTickMsg struct{}
	statusMsg     struct{ st *ipc.Status }
)

func waitForItem(ch <-chan *ipc.Item) tea.Cmd {
	return func() tea.Msg {
		it, ok := <-ch
		if !ok {
			return streamClosedMsg{}
		}
		return itemMsg{item: it}
	}
}

func sendText(cl tuiClient, text string) tea.Cmd {
	return func() tea.Msg { return sentMsg{err: cl.Send("text", []byte(text))} }
}

func copyText(cl tuiClient, text string) tea.Cmd {
	return func() tea.Msg {
		return copiedMsg{err: cl.Copy("text", []byte(text)), label: "copied text to clipboard"}
	}
}

func copyImage(cl tuiClient, path string) tea.Cmd {
	return func() tea.Msg {
		if path == "" {
			return copiedMsg{err: errors.New("no image file for this item")}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return copiedMsg{err: err}
		}
		return copiedMsg{err: cl.Copy("image", data), label: "copied image to clipboard"}
	}
}

func pasteLatest(cl tuiClient) tea.Cmd {
	return func() tea.Msg {
		return copiedMsg{err: cl.Paste(), label: "pasted latest item to clipboard"}
	}
}

func saveImage(src, name string) tea.Cmd {
	return func() tea.Msg {
		if src == "" {
			return savedMsg{err: errors.New("no image file for this item")}
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return savedMsg{err: err}
		}
		if name == "" {
			name = "image.png"
		}
		dst := filepath.Join(saveDir(), name)
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return savedMsg{err: err}
		}
		return savedMsg{path: dst}
	}
}

func openExternal(path string) tea.Cmd {
	return func() tea.Msg {
		if path == "" {
			return openedMsg{err: errors.New("no image file for this item")}
		}
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("open", path)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
		default:
			cmd = exec.Command("xdg-open", path)
		}
		return openedMsg{err: cmd.Start()}
	}
}

func tickStatus() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return statusTickMsg{} })
}

func fetchStatus(cl tuiClient) tea.Cmd {
	return func() tea.Msg {
		st, err := cl.Status()
		if err != nil {
			return statusMsg{}
		}
		return statusMsg{st: st}
	}
}

// ---- small helpers ---------------------------------------------------------

func senderLabel(env *wire.Envelope) string {
	if env.DeviceName != "" {
		return env.DeviceName
	}
	return shortID(env.Sender)
}

func shortID(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:10] + "…"
}

func shortHash(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return "now"
	}
	d := time.Since(t)
	switch {
	case d < 5*time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return t.Format("Jan 2 15:04")
	}
}

func humanSize(n uint64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// wrap is a minimal word-wrapper for text bubble bodies so long lines don't
// blow past the viewport width.
func wrap(s string, width int) string {
	if width < 8 {
		width = 8
	}
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		col := 0
		for j, word := range strings.Fields(line) {
			wl := len([]rune(word))
			if j > 0 {
				if col+1+wl > width {
					out.WriteByte('\n')
					col = 0
				} else {
					out.WriteByte(' ')
					col++
				}
			}
			out.WriteString(word)
			col += wl
		}
	}
	return out.String()
}

// saveDir picks a sensible directory for `s` (save image): ~/Downloads when it
// exists, else the OS temp dir.
func saveDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		dl := filepath.Join(home, "Downloads")
		if fi, err := os.Stat(dl); err == nil && fi.IsDir() {
			return dl
		}
	}
	return os.TempDir()
}
