package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

// Layout constants: the non-viewport chrome is header(1) + toast(1) +
// composer(label 1 + textarea 3) + hint(1) = 7 lines.
const (
	composerTextHeight = 3
	chromeLines        = 1 + 1 + (1 + composerTextHeight) + 1
)

var errNoLocalImage = errors.New("image not materialized on disk")

// ---- styles ----

var (
	colAccent = lipgloss.Color("205") // pink
	colMine   = lipgloss.Color("42")  // green
	colTheirs = lipgloss.Color("39")  // blue
	colSelect = lipgloss.Color("214") // amber
	colDim    = lipgloss.Color("244")
	colWarn   = lipgloss.Color("208")

	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).
			Background(lipgloss.Color("57")).Padding(0, 1)
	hintStyle  = lipgloss.NewStyle().Foreground(colDim)
	toastStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(colWarn).Padding(0, 1)
	metaStyle  = lipgloss.NewStyle().Foreground(colDim)
	nudgeStyle = lipgloss.NewStyle().Foreground(colAccent).Italic(true)
	labelMine  = lipgloss.NewStyle().Bold(true).Foreground(colMine)
	labelTheir = lipgloss.NewStyle().Bold(true).Foreground(colTheirs)
)

// resize recomputes component dimensions for a w×h terminal.
func (m *model) resize(w, h int) {
	if w < 20 {
		w = 20
	}
	if h < chromeLines+3 {
		h = chromeLines + 3
	}
	m.width, m.height = w, h
	m.ta.SetWidth(w)
	m.ta.SetHeight(composerTextHeight)
	m.vp.Width = w
	m.vp.Height = h - chromeLines
}

// bubbleWidth is the max width of a chat bubble box.
func (m model) bubbleWidth() int {
	w := m.vp.Width - 6
	if w > 64 {
		w = 64
	}
	if w < 16 {
		w = 16
	}
	return w
}

// rebuild re-renders all bubbles into the viewport. When stickBottom is true
// (a new message, or we were already at the bottom) it scrolls to the newest.
func (m *model) rebuild(stickBottom bool) {
	var lines []string
	starts := make([]int, len(m.bubbles))
	for i, b := range m.bubbles {
		if i > 0 {
			lines = append(lines, "") // blank separator between bubbles
		}
		starts[i] = len(lines)
		block := m.renderBubble(b, i == m.selected)
		lines = append(lines, strings.Split(block, "\n")...)
	}
	if len(m.bubbles) == 0 {
		lines = []string{metaStyle.Render("  No messages yet. Type below and press Enter to send.")}
	}
	m.lineStarts = starts
	m.contentLines = len(lines)
	m.vp.SetContent(strings.Join(lines, "\n"))
	if stickBottom {
		m.vp.GotoBottom()
	}
}

// scrollToSelection nudges the viewport so the selected bubble is visible.
func (m *model) scrollToSelection() {
	if m.selected < 0 || m.selected >= len(m.lineStarts) {
		return
	}
	top := m.lineStarts[m.selected]
	bottom := m.contentLines - 1
	if m.selected < len(m.lineStarts)-1 {
		bottom = m.lineStarts[m.selected+1] - 2 // exclude the blank separator
	}
	if top < m.vp.YOffset {
		m.vp.SetYOffset(top)
	} else if bottom > m.vp.YOffset+m.vp.Height-1 {
		m.vp.SetYOffset(bottom - m.vp.Height + 1)
	}
}

// renderBubble renders one chat bubble block (possibly multiple lines).
func (m model) renderBubble(b bubble, selected bool) string {
	env := b.item.Envelope

	// Header: sender label · relative time.
	var label string
	var labelStyle lipgloss.Style
	if b.mine {
		label, labelStyle = "you", labelMine
	} else {
		label = env.DeviceName
		if label == "" {
			label = shortID(env.Sender)
		}
		labelStyle = labelTheir
	}
	head := labelStyle.Render(label) + metaStyle.Render("  ·  "+humanTime(env.TS))

	// Body.
	var body string
	switch env.Type {
	case wire.TypeImage:
		body = m.renderImageBody(b)
	case wire.TypeFile:
		body = m.renderFileBody(b)
	default:
		body = env.Text
	}

	inner := head + "\n" + body
	if !b.accepted { // notify-first: nudge until the user copies it
		inner += "\n" + nudgeStyle.Render("press y to copy")
	}

	border := colTheirs
	if b.mine {
		border = colMine
	}
	if selected {
		border = colSelect
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(m.bubbleWidth()).
		Render(inner)

	align := lipgloss.Left
	if b.mine {
		align = lipgloss.Right
	}
	return lipgloss.NewStyle().Width(m.vp.Width).Align(align).Render(box)
}

func labelThir(env wire.Envelope) lipgloss.Style { return labelTheir }

// renderImageBody renders the image placeholder + action affordances. Inline
// terminal-graphics rendering is a documented stub (see README); the metadata
// line and the y/s/o actions are the usable surface for Phase 0.
func (m model) renderImageBody(b bubble) string {
	env := b.item.Envelope
	name := imageName(b)
	var dims, size string
	if env.Blob != nil {
		dims = fmt.Sprintf("%d×%d", env.Blob.W, env.Blob.H)
		size = humanSize(env.Blob.Size)
	}
	head := fmt.Sprintf("🖼  %s  %s · %s", name, dims, size)
	hint := metaStyle.Render("[inline preview stubbed — y copy · s save · o open]")
	return head + "\n" + hint
}

// renderFileBody renders a file item: name + size, plus its landing path. A
// file has no clipboard image form; `y` copies its path as text (SPEC §2).
func (m model) renderFileBody(b bubble) string {
	env := b.item.Envelope
	name := env.Filename
	if name == "" && b.item.LocalPath != "" {
		name = filepath.Base(b.item.LocalPath)
	}
	if name == "" {
		name = "file"
	}
	var size string
	if env.Blob != nil {
		size = " · " + humanSize(env.Blob.Size)
	}
	head := fmt.Sprintf("📄  %s%s", name, size)
	hint := metaStyle.Render("[file — y copy path · s save · o open]")
	return head + "\n" + hint
}

// ---- top-level view ----

func (m model) View() string {
	header := m.renderHeader()
	body := m.vp.View()
	toast := m.renderToast()
	composer := m.renderComposer()
	hint := m.renderHint()
	return lipgloss.JoinVertical(lipgloss.Left, header, body, toast, composer, hint)
}

func (m model) renderHeader() string {
	room := m.opts.Room
	fp := "—"
	conn := "○ starting…"
	auto := "notify"
	clip := "unavailable"
	if m.haveStatus {
		if m.status.Room != "" {
			room = m.status.Room
		}
		fp = shortFP(m.status.Fingerprint)
		auto = m.status.AutoCopy
		if m.status.Clipboard {
			clip = "available"
		}
		if m.status.Connected {
			conn = "● " + m.status.Server
		} else {
			conn = "○ not connected"
		}
	}
	if room == "" {
		room = "default"
	}
	line := fmt.Sprintf("room:%s · %s · %s · auto_copy:%s · clipboard:%s", room, fp, conn, auto, clip)
	return headerStyle.Inline(true).MaxWidth(m.width).Width(m.width).Render(line)
}

func (m model) renderToast() string {
	if m.quitting {
		return toastStyle.Inline(true).MaxWidth(m.width).Render(
			"⚑ clear this session? [t]ransient / [a]ll incl sinks / [n]o")
	}
	if m.toast != "" {
		return toastStyle.Inline(true).MaxWidth(m.width).Render("⚑ " + m.toast)
	}
	// Idle: a subtle status line.
	mode := "browse"
	if m.focused {
		mode = "compose"
	}
	s := fmt.Sprintf("%d message(s) · %s mode", len(m.bubbles), mode)
	if m.haveStatus && m.status.AutoCopy == "notify" {
		s += " · notify-first: press y to copy received items"
	}
	return metaStyle.Inline(true).MaxWidth(m.width).Render("  " + s)
}

func (m model) renderComposer() string {
	var label string
	if m.focused {
		label = lipgloss.NewStyle().Bold(true).Foreground(colAccent).Render("▶ compose")
	} else {
		label = metaStyle.Render("  composer (press i or Esc to type)")
	}
	return lipgloss.JoinVertical(lipgloss.Left, label, m.ta.View())
}

func (m model) renderHint() string {
	var h string
	if m.quitting {
		h = "t transient · a all incl sinks · n no · ctrl+c force quit"
	} else if m.focused {
		h = "enter send · esc browse · ctrl+c quit"
	} else {
		h = "j/k select · y copy · s save · o open · g/G top/bottom · i compose · q quit"
	}
	return hintStyle.Inline(true).MaxWidth(m.width).Render(h)
}

// ---- helpers ----

func humanTime(tsMillis uint64) string {
	if tsMillis == 0 {
		return "now"
	}
	d := time.Since(time.UnixMilli(int64(tsMillis)))
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
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func humanSize(n uint64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// shortFP shortens an SSH SHA256 fingerprint for the header.
func shortFP(fp string) string {
	if fp == "" {
		return "—"
	}
	body := fp
	if i := strings.Index(fp, ":"); i >= 0 {
		body = fp[i+1:]
	}
	if len(body) > 12 {
		body = body[:12] + "…"
	}
	return "🔑" + body
}

// shortID is a compact peer label when no device name is present.
func shortID(fp string) string {
	body := fp
	if i := strings.Index(fp, ":"); i >= 0 {
		body = fp[i+1:]
	}
	if len(body) > 8 {
		body = body[:8]
	}
	if body == "" {
		return "peer"
	}
	return "peer-" + body
}

func imageName(b bubble) string {
	if b.item.LocalPath != "" {
		return filepath.Base(b.item.LocalPath)
	}
	if b.item.Envelope.Blob != nil && b.item.Envelope.Blob.Hash != "" {
		return "image-" + b.item.Envelope.Blob.Hash[:min(8, len(b.item.Envelope.Blob.Hash))] + ".png"
	}
	return "image.png"
}

// saveImage copies a materialized blob to ~/Downloads (or the cwd) and returns
// the destination path.
func saveImage(src string, blob *wire.Blob) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	name := "room-image.png"
	if blob != nil && blob.Hash != "" {
		name = "room-" + blob.Hash[:min(8, len(blob.Hash))] + ".png"
	}
	dir := destDir()
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}

func destDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		dl := filepath.Join(home, "Downloads")
		if fi, err := os.Stat(dl); err == nil && fi.IsDir() {
			return dl
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return os.TempDir()
}
