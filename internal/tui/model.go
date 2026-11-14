package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
)

type toastLevel int

const (
	levelInfo toastLevel = iota
	levelOK
	levelWarn
	levelErr
)

type toast struct {
	text  string
	level toastLevel
	seq   int
}

// Root messages.
type (
	toastMsg struct {
		level toastLevel
		text  string
	}
	toastExpireMsg struct{ seq int }
	tickMsg        struct{ seq int }
	spinMsg        struct{}
)

// jobResult is implemented by the result messages of background jobs
// (checks, ranking, tag refreshes, list downloads): the root forgets the
// job when its result arrives.
type jobResult interface{ jobID() int }

const spinEvery = 120 * time.Millisecond

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Model is the root Bubble Tea model.
type Model struct {
	ctx    context.Context
	cancel context.CancelFunc
	opts   Options
	d      *db.DB
	cfg    *config.Config

	renderer *lipgloss.Renderer
	styles   *Styles
	theme    string

	width, height int

	views  []view
	active int
	dash   *dashboardView
	prox   *proxiesView
	hist   *historyView
	tagv   *tagsView
	scans  *scansView
	lists  *listsView
	conf   *configView

	pal  *palette
	help *helpOverlay

	toast    toast
	toastSeq int
	jobs     map[int]string // running background jobs: id -> label
	jobSeq   int
	spin     int
	spinning bool

	refreshEvery time.Duration
	tickSeq      int
	confirming   bool // "quit anyway?" while jobs run
	quitting     bool

	tagLabels []string // tag labels for completion (loaded with the palette)

	// tick is tea.Tick; tests replace it so no timer ever runs.
	tick func(time.Duration, func(time.Time) tea.Msg) tea.Cmd
}

// Init loads the start tab and starts the auto-refresh.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.SetWindowTitle("SilkRouter"), m.activeView().refresh(m), m.scheduleTick()}
	if ws := m.cfg.Warnings(); len(ws) > 0 {
		cmds = append(cmds, m.notify(levelWarn, "config: %s", ws[0]))
	}
	if bad := m.prox.setColumns(m); len(bad) > 0 {
		cmds = append(cmds, m.notify(levelWarn, "%s", columnsWarning(bad)))
	}
	return tea.Batch(cmds...)
}

func (m *Model) activeView() view { return m.views[m.active] }

func (m *Model) bodyHeight() int { return max(1, m.height-3) }

func (m *Model) pageSize() int { return max(10, m.cfg.TUI.PageSize) }

// layout resizes every view (and re-applies styles after a theme change).
func (m *Model) layout() {
	h := m.bodyHeight()
	for _, v := range m.views {
		v.resize(m, m.width, h)
	}
	m.pal.resize(m)
	if m.help.open {
		m.help.layout(m)
	}
}

// Update routes messages: keys and mouse to the active view (after the
// overlays and global keys), everything else to every view.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case tea.KeyMsg:
		return m, m.handleKey(msg)
	case tea.MouseMsg:
		return m, m.handleMouse(msg)
	case toastMsg:
		return m, m.notify(msg.level, "%s", msg.text)
	case toastExpireMsg:
		if msg.seq == m.toast.seq {
			m.toast = toast{}
		}
		return m, nil
	case tickMsg:
		if msg.seq != m.tickSeq {
			return m, nil // superseded by a restart
		}
		var cmd tea.Cmd
		if v := m.activeView(); !v.capturing() && !m.pal.open && !m.help.open {
			cmd = v.refresh(m)
		}
		return m, tea.Batch(cmd, m.scheduleTick())
	case spinMsg:
		m.spin++
		if len(m.jobs) == 0 {
			m.spinning = false
			return m, nil
		}
		return m, m.tick(spinEvery, func(time.Time) tea.Msg { return spinMsg{} })
	}
	var cmds []tea.Cmd
	if jr, ok := msg.(jobResult); ok {
		delete(m.jobs, jr.jobID())
	}
	cmds = append(cmds, m.handleResult(msg))
	for _, v := range m.views {
		cmds = append(cmds, v.update(m, msg))
	}
	cmds = append(cmds, m.pal.update(m, msg))
	return m, tea.Batch(cmds...)
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	// Bubble Tea delivers runes that arrive in one read (fast typing over
	// SSH, scripted input) as one message: handle them one by one so
	// ":", "/", "q", ... keep working. Pastes stay whole.
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
		cmds := make([]tea.Cmd, 0, len(msg.Runes))
		for _, r := range msg.Runes {
			cmds = append(cmds, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt}))
		}
		return tea.Batch(cmds...)
	}
	if msg.String() == "ctrl+c" {
		if m.confirming {
			return m.quit()
		}
		return m.requestQuit(false)
	}
	if m.confirming {
		m.confirming = false
		switch msg.String() {
		case "y", "Y":
			return m.quit()
		}
		return m.notify(levelInfo, "still here")
	}
	if m.help.open {
		return m.help.key(m, msg)
	}
	if m.pal.open {
		return m.pal.key(m, msg)
	}
	v := m.activeView()
	if v.capturing() {
		return v.key(m, msg)
	}
	switch {
	case key.Matches(msg, keys.Quit):
		return m.requestQuit(false)
	case key.Matches(msg, keys.Palette):
		return m.pal.show(m, "")
	case key.Matches(msg, keys.Help):
		return m.help.show(m)
	case key.Matches(msg, keys.NextTab):
		return m.switchTab(m.active + 1)
	case key.Matches(msg, keys.PrevTab):
		return m.switchTab(m.active - 1)
	case key.Matches(msg, keys.Refresh):
		return m.reloadActive()
	case key.Matches(msg, keys.Tabs):
		return m.switchTab(int(msg.Runes[0] - '1'))
	}
	return v.key(m, msg)
}

func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if m.confirming || m.pal.open {
		return nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		if msg.Action != tea.MouseActionPress {
			return nil
		}
		d := 3
		if msg.Button == tea.MouseButtonWheelUp {
			d = -3
		}
		if m.help.open {
			m.help.scroll(d)
			return nil
		}
		return m.activeView().scroll(m, d)
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress || m.help.open {
			return nil
		}
		if msg.Y == 0 {
			_, _, spans := m.tabSpans()
			for i, sp := range spans {
				if msg.X >= sp[0] && msg.X < sp[1] {
					return m.switchTab(i)
				}
			}
			return nil
		}
		if y := msg.Y - 1; y >= 0 && y < m.bodyHeight() {
			return m.activeView().click(m, msg.X, y)
		}
	}
	return nil
}

// switchTab activates tab i (wrapping) and reloads it.
func (m *Model) switchTab(i int) tea.Cmd {
	n := len(m.views)
	i = ((i % n) + n) % n
	if i == m.active {
		return nil
	}
	m.active = i
	m.help.open = false
	return m.activeView().refresh(m)
}

func (m *Model) reloadActive() tea.Cmd {
	m.tagLabels = nil
	return tea.Batch(m.activeView().refresh(m), m.notify(levelInfo, "reloaded %s", strings.ToLower(m.activeView().title())))
}

// notify shows a toast in the status bar for a few seconds.
func (m *Model) notify(level toastLevel, format string, a ...any) tea.Cmd {
	m.toastSeq++
	m.toast = toast{text: fmt.Sprintf(format, a...), level: level, seq: m.toastSeq}
	ttl := 4 * time.Second
	if level >= levelWarn {
		ttl = 8 * time.Second
	}
	seq := m.toastSeq
	return m.tick(ttl, func(time.Time) tea.Msg { return toastExpireMsg{seq: seq} })
}

func (m *Model) scheduleTick() tea.Cmd {
	m.tickSeq++
	if m.refreshEvery <= 0 {
		return nil
	}
	seq := m.tickSeq
	return m.tick(m.refreshEvery, func(time.Time) tea.Msg { return tickMsg{seq: seq} })
}

// startJob registers a background job (shown in the status bar and
// guarded by tui.confirm_quit) and starts the spinner.
func (m *Model) startJob(label string) (int, tea.Cmd) {
	m.jobSeq++
	m.jobs[m.jobSeq] = label
	var cmd tea.Cmd
	if !m.spinning {
		m.spinning = true
		cmd = m.tick(spinEvery, func(time.Time) tea.Msg { return spinMsg{} })
	}
	return m.jobSeq, cmd
}

func (m *Model) jobsText() string {
	if len(m.jobs) == 1 {
		for _, l := range m.jobs {
			return l
		}
	}
	labels := make([]string, 0, len(m.jobs))
	for _, l := range m.jobs {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	return fmt.Sprintf("%d jobs (%s)", len(labels), strings.Join(labels, ", "))
}

// requestQuit quits, or asks first while jobs run and tui.confirm_quit is
// set (force skips the question).
func (m *Model) requestQuit(force bool) tea.Cmd {
	if !force && len(m.jobs) > 0 && m.cfg.TUI.ConfirmQuit {
		m.confirming = true
		return nil
	}
	return m.quit()
}

func (m *Model) quit() tea.Cmd {
	m.quitting = true
	m.cancel()
	return tea.Quit
}

// setTheme switches the theme live.
func (m *Model) setTheme(name string) error {
	t, ok := themeByName(name)
	if !ok {
		return fmt.Errorf("unknown theme %q (want one of: %s)", name, strings.Join(ThemeNames(), ", "))
	}
	m.theme = t.Name
	m.styles = newStyles(m.renderer, t)
	m.help.cacheKey = ""
	m.layout()
	return nil
}

// ---- rendering ---------------------------------------------------------------

// View renders tab bar, body, status bar and the bottom line.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	h := m.bodyHeight()
	var body string
	if m.help.open {
		body = m.help.view(m, m.width, h)
	} else {
		body = m.activeView().render(m)
	}
	return strings.Join([]string{
		fitLines(m.renderTabs(), m.width, 1),
		fitLines(body, m.width, h),
		fitLines(m.renderStatus(), m.width, 1),
		fitLines(m.renderBottom(), m.width, 1),
	}, "\n")
}

func (m *Model) tabTitles(form int) []string {
	out := make([]string, len(m.views))
	for i, v := range m.views {
		t := v.title()
		switch form {
		case 1:
			if len(t) > 4 {
				t = t[:4]
			}
		case 2:
			t = ""
		}
		out[i] = strings.TrimSpace(fmt.Sprintf("%d %s", i+1, t))
	}
	return out
}

// tabSpans renders the brand and the tab labels (shortened until they
// fit) and returns each tab's x range, for mouse clicks.
func (m *Model) tabSpans() (brand string, tabs []string, spans [][2]int) {
	s := m.styles
	brand = s.Brand.Render("◆ SilkRouter")
	if s.plain {
		brand = "SilkRouter |"
	}
	for form := 0; form < 3; form++ {
		tabs, spans = nil, nil
		x := lipgloss.Width(brand) + 1
		for i, t := range m.tabTitles(form) {
			var r string
			switch {
			case s.plain && i == m.active:
				r = "[" + t + "]"
			case s.plain:
				r = " " + t + " "
			case i == m.active:
				r = s.TabActive.Render(t)
			default:
				r = s.TabInactive.Render(t)
			}
			w := lipgloss.Width(r)
			tabs = append(tabs, r)
			spans = append(spans, [2]int{x, x + w})
			x += w
		}
		if x <= m.width {
			break
		}
	}
	return brand, tabs, spans
}

func (m *Model) renderTabs() string {
	brand, tabs, _ := m.tabSpans()
	line := brand + " " + strings.Join(tabs, "")
	if v := m.opts.Version; v != "" {
		right := m.styles.Faint.Render("v" + strings.TrimPrefix(v, "v"))
		if gap := m.width - lipgloss.Width(line) - lipgloss.Width(right) - 1; gap > 0 {
			line += strings.Repeat(" ", gap) + right
		}
	}
	return line
}

func (m *Model) toastView(maxW int) string {
	s := m.styles
	text := truncate(m.toast.text, max(10, maxW-2))
	if s.plain {
		prefix := map[toastLevel]string{levelInfo: "", levelOK: "ok: ", levelWarn: "warning: ", levelErr: "error: "}[m.toast.level]
		return "| " + prefix + text
	}
	switch m.toast.level {
	case levelOK:
		return s.ToastOK.Render(text)
	case levelWarn:
		return s.ToastWarn.Render(text)
	case levelErr:
		return s.ToastErr.Render(text)
	}
	return s.ToastInfo.Render(text)
}

func (m *Model) renderStatus() string {
	s := m.styles
	if m.pal.open {
		return m.pal.statusLine(m)
	}
	v := m.activeView()
	title := strings.ToUpper(v.title())
	pill := s.ModePill.Render(title)
	if s.plain {
		pill = "[" + title + "]"
	}
	segs := v.status(m)
	if len(m.jobs) > 0 {
		segs = append(segs, spinFrames[m.spin%len(spinFrames)]+" "+m.jobsText())
	}
	text := " " + strings.Join(segs, " · ") + " "
	right := ""
	if m.toast.text != "" {
		right = m.toastView(m.width * 6 / 10)
	}
	avail := m.width - lipgloss.Width(pill) - lipgloss.Width(right)
	if avail < 0 {
		right = ""
		avail = m.width - lipgloss.Width(pill)
	}
	return pill + s.StatusBar.Render(pad(text, max(0, avail), false)) + right
}

// hint is one entry of the bottom key-hint line.
type hint struct{ key, desc string }

func (m *Model) renderBottom() string {
	s := m.styles
	switch {
	case m.confirming:
		return s.Warn.Render(fmt.Sprintf(" %s running: quit anyway? [y/N]", m.jobsText()))
	case m.pal.open:
		return m.pal.input.View()
	case m.help.open:
		return m.hintLine([]hint{{"↑↓/pgup/pgdn", "scroll"}, {"esc/?/q", "close"}})
	}
	v := m.activeView()
	if f := v.footer(m); f != "" {
		return f
	}
	return m.hintLine(append(v.hints(), hint{":", "commands"}, hint{"?", "help"}, hint{"q", "quit"}))
}

func (m *Model) hintLine(hs []hint) string {
	s := m.styles
	parts := make([]string, 0, len(hs))
	for _, h := range hs {
		parts = append(parts, s.HintKey.Render(h.key)+" "+s.Hint.Render(h.desc))
	}
	return " " + strings.Join(parts, s.Faint.Render("  ·  "))
}
