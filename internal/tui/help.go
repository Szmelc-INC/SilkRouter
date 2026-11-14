package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// helpOverlay is the '?' overlay: the active tab's keys, the global keys
// and every palette command, rendered as markdown with glamour
// (tui.markdown) or as aligned plain text.
type helpOverlay struct {
	open     bool
	vp       viewport.Model
	cacheKey string
	content  string
}

func (h *helpOverlay) show(m *Model) tea.Cmd {
	h.open = true
	h.vp = viewport.New(0, 0)
	h.layout(m)
	return nil
}

func (h *helpOverlay) boxWidth(m *Model) int { return max(20, min(m.width-2, 104)) }

func (h *helpOverlay) layout(m *Model) {
	w := h.boxWidth(m)
	inner := w - 4 // border + padding
	key := fmt.Sprintf("%s|%d|%s|%v", m.activeView().name(), inner, m.theme, m.cfg.TUI.Markdown)
	if key != h.cacheKey {
		h.content = m.helpContent(inner)
		h.cacheKey = key
	}
	h.vp.Width = inner
	h.vp.Height = max(1, m.bodyHeight()-2)
	h.vp.SetContent(h.content)
}

func (h *helpOverlay) scroll(d int) {
	if d < 0 {
		h.vp.ScrollUp(-d)
	} else {
		h.vp.ScrollDown(d)
	}
}

func (h *helpOverlay) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "?", "q", "enter":
		h.open = false
		return nil
	case ":":
		h.open = false
		return m.pal.show(m, "")
	}
	switch {
	case key.Matches(msg, keys.Up):
		h.vp.ScrollUp(1)
	case key.Matches(msg, keys.Down):
		h.vp.ScrollDown(1)
	case key.Matches(msg, keys.PageUp):
		h.vp.PageUp()
	case key.Matches(msg, keys.PageDown), msg.String() == " ":
		h.vp.PageDown()
	case key.Matches(msg, keys.HalfUp):
		h.vp.HalfPageUp()
	case key.Matches(msg, keys.HalfDown):
		h.vp.HalfPageDown()
	case key.Matches(msg, keys.Top):
		h.vp.GotoTop()
	case key.Matches(msg, keys.Bottom):
		h.vp.GotoBottom()
	}
	return nil
}

func (h *helpOverlay) view(m *Model, w, height int) string {
	box := m.styles.Overlay.Width(h.boxWidth(m) - 2).Render(h.vp.View())
	return lipgloss.Place(w, height, lipgloss.Center, lipgloss.Top, box)
}

type helpSection struct {
	title string
	rows  [][2]string
}

func bindingRows(bs []key.Binding) [][2]string {
	rows := make([][2]string, 0, len(bs))
	for _, b := range bs {
		rows = append(rows, [2]string{b.Help().Key, b.Help().Desc})
	}
	return rows
}

// helpSections is the help content, shared by both renderings.
func (m *Model) helpSections() (title, about string, secs []helpSection) {
	v := m.activeView()
	secs = append(secs, helpSection{v.title() + " keys", bindingRows(v.help())})
	if v != view(m.dash) {
		nav := navKeys
		if v == view(m.scans) {
			nav = append([]key.Binding{keys.UpNoK}, navKeys[1:]...)
		}
		secs = append(secs, helpSection{"Moving around", bindingRows(nav)})
	}
	secs = append(secs, helpSection{"Everywhere", bindingRows(globalKeys)})
	var pal [][2]string
	for _, c := range commandList() {
		name := ":" + c.name
		if c.args != "" {
			name += " " + c.args
		}
		desc := c.help
		if len(c.aliases) > 0 {
			desc += " (also :" + strings.Join(c.aliases, ", :") + ")"
		}
		pal = append(pal, [2]string{name, desc})
	}
	secs = append(secs, helpSection{"Command palette", pal})
	secs = append(secs, helpSection{"Mouse", [][2]string{
		{"wheel", "scroll the table / pane"},
		{"click", "select a row (click it again to open it); click a tab to switch"},
	}})
	return "SilkRouter · " + v.title(), v.about(), secs
}

func (m *Model) helpContent(width int) string {
	title, about, secs := m.helpSections()
	if m.cfg.TUI.Markdown {
		if out, err := m.helpMarkdown(title, about, secs, width); err == nil {
			return out
		}
	}
	return m.helpPlain(title, about, secs, width)
}

func (m *Model) helpMarkdown(title, about string, secs []helpSection, width int) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", title, about)
	for _, sec := range secs {
		fmt.Fprintf(&b, "## %s\n\n", sec.title)
		for _, r := range sec.rows {
			fmt.Fprintf(&b, "- `%s` — %s\n", r[0], r[1])
		}
		b.WriteString("\n")
	}
	b.WriteString("Tab completes palette commands and their arguments; ↑/↓ recall earlier commands. " +
		"Set `tui.markdown = false` for plain-text help.\n")
	style := m.styles.theme.Glamour
	profile := m.renderer.ColorProfile()
	if profile == termenv.Ascii {
		style = "ascii"
	}
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width),
		glamour.WithColorProfile(profile))
	if err != nil {
		return "", err
	}
	out, err := r.Render(b.String())
	if err != nil {
		return "", err
	}
	return strings.Trim(out, "\n"), nil
}

func (m *Model) helpPlain(title, about string, secs []helpSection, width int) string {
	s := m.styles
	var b strings.Builder
	b.WriteString(s.Title.Render(title) + "\n\n")
	for _, l := range wrapText(strings.ReplaceAll(about, "`", ""), width) {
		b.WriteString(s.Text.Render(l) + "\n")
	}
	for _, sec := range secs {
		b.WriteString("\n" + s.Accent.Render(sec.title) + "\n")
		kw := 0
		for _, r := range sec.rows {
			kw = max(kw, len([]rune(r[0])))
		}
		kw = min(kw, max(10, width/3))
		for _, r := range sec.rows {
			lines := wrapText(r[1], max(10, width-kw-4))
			for i, l := range lines {
				k := ""
				if i == 0 {
					k = r[0]
				}
				b.WriteString("  " + s.HintKey.Render(pad(k, kw, false)) + "  " + s.Text.Render(l) + "\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// wrapText wraps plain text at word boundaries.
func wrapText(s string, width int) []string {
	var out []string
	line := ""
	for _, w := range strings.Fields(s) {
		switch {
		case line == "":
			line = w
		case len([]rune(line))+1+len([]rune(w)) > width:
			out = append(out, line)
			line = w
		default:
			line += " " + w
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}
