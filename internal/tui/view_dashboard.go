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

	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

// sessionInfo is the shell session the TUI runs in ('px on' exports
// SILKROUTER_ACTIVE, SILKROUTER_PROXY and SILKROUTER_SESSION).
type sessionInfo struct {
	active    bool
	proxyURL  string
	sessionID string
	proxy     *db.Proxy
	uses      int
}

type dashData struct {
	stats   *db.Stats
	tags    []db.TagCount
	runs    []*db.ScanRun
	cmds    []db.UsageGroup
	session sessionInfo
}

type dashMsg struct {
	seq  int
	data *dashData
	err  error
}

type dashboardView struct {
	baseView
	data *dashData
	err  error
	seq  int
	off  int // scroll offset
}

func newDashboardView() *dashboardView { return &dashboardView{} }

func (v *dashboardView) name() string  { return TabDashboard }
func (v *dashboardView) title() string { return "Dashboard" }
func (v *dashboardView) about() string {
	return "An overview of the database: proxies by status and scheme, the latency of the alive ones, facts, " +
		"tags, the latest scan runs, the most used commands of the week and the proxy of the current shell session " +
		"(`SILKROUTER_ACTIVE` / `SILKROUTER_PROXY`, set by `px on`)."
}

func loadSession(ctx context.Context, d *db.DB, env func(string) string) sessionInfo {
	s := sessionInfo{proxyURL: env("SILKROUTER_PROXY"), sessionID: env("SILKROUTER_SESSION")}
	a := strings.ToLower(strings.TrimSpace(env("SILKROUTER_ACTIVE")))
	s.active = (a != "" && a != "0" && a != "false" && a != "off") || (a == "" && s.proxyURL != "")
	if s.proxyURL != "" {
		if p, err := d.GetProxyByURL(ctx, s.proxyURL); err == nil {
			s.proxy = p
		}
	}
	if s.sessionID != "" {
		s.uses, _ = d.CountUsage(ctx, db.UsageFilter{SessionID: s.sessionID})
	}
	return s
}

func (v *dashboardView) refresh(m *Model) tea.Cmd {
	v.seq++
	seq := v.seq
	d, ctx, env := m.d, m.ctx, m.opts.Getenv
	return func() tea.Msg {
		out := &dashData{}
		var err error
		if out.stats, err = d.Stats(ctx); err != nil {
			return dashMsg{seq: seq, err: err}
		}
		out.tags, _ = d.TagCounts(ctx)
		out.runs, _ = d.ListScanRuns(ctx, 5)
		out.cmds, _ = d.UsageStats(ctx, []string{"cmd"}, db.UsageFilter{Since: time.Now().Add(-7 * 24 * time.Hour)}, "uses", 6)
		out.session = loadSession(ctx, d, env)
		return dashMsg{seq: seq, data: out}
	}
}

func (v *dashboardView) update(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case dashMsg:
		if msg.seq == v.seq {
			v.err = msg.err
			if msg.data != nil {
				v.data = msg.data
			}
		}
	case rankDoneMsg, retagDoneMsg, checkDoneMsg, tagsChangedMsg, scanEndedMsg:
		if m.activeView() == view(v) {
			return v.refresh(m)
		}
	}
	return nil
}

func (v *dashboardView) resize(m *Model, w, h int) { v.w, v.h = w, h }

func (v *dashboardView) lines(m *Model) []string { return strings.Split(v.content(m), "\n") }

func (v *dashboardView) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	n := len(v.lines(m))
	switch {
	case key.Matches(msg, keys.Up):
		v.off--
	case key.Matches(msg, keys.Down):
		v.off++
	case key.Matches(msg, keys.PageUp):
		v.off -= v.h
	case key.Matches(msg, keys.PageDown):
		v.off += v.h
	case key.Matches(msg, keys.Top):
		v.off = 0
	case key.Matches(msg, keys.Bottom):
		v.off = n
	}
	v.off = min(max(0, v.off), max(0, n-v.h))
	return nil
}

func (v *dashboardView) scroll(m *Model, d int) tea.Cmd {
	n := len(v.lines(m))
	v.off = min(max(0, v.off+d), max(0, n-v.h))
	return nil
}

func (v *dashboardView) render(m *Model) string {
	lines := v.lines(m)
	start := min(v.off, max(0, len(lines)-v.h))
	return strings.Join(lines[start:], "\n")
}

// content lays the cards out in 1-3 columns depending on the width.
func (v *dashboardView) content(m *Model) string {
	s := m.styles
	if v.err != nil {
		return s.Error.Render("  " + v.err.Error())
	}
	if v.data == nil {
		return s.Muted.Render("  loading…")
	}
	cols := 1
	switch {
	case v.w >= 150:
		cols = 3
	case v.w >= 84:
		cols = 2
	}
	cw := (v.w - (cols - 1)) / cols
	inner := cw - 4 // border + padding
	type card struct{ title, body string }
	cards := []card{
		{"Proxies", v.proxiesCard(m, inner)},
		{"Alive latency", v.latencyCard(m, inner)},
		{"Shell session", v.sessionCard(m, inner)},
		{"Schemes", v.schemesCard(m, inner)},
		{"Facts", v.factsCard(m, inner)},
		{"Tags", v.tagsCard(m, inner)},
		{"Recent scans", v.scansCard(m, inner)},
		{"Top commands (7 days)", v.usageCard(m, inner)},
	}
	var rows []string
	for i := 0; i < len(cards); i += cols {
		group := cards[i:min(i+cols, len(cards))]
		h := 0
		for _, c := range group {
			h = max(h, strings.Count(c.body, "\n")+2)
		}
		var rendered []string
		for j, c := range group {
			if j > 0 {
				rendered = append(rendered, " ")
			}
			rendered = append(rendered, s.Card.Width(cw-2).Height(h).Render(s.CardTitle.Render(c.title)+"\n"+c.body))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, rendered...))
	}
	return strings.Join(rows, "\n")
}

// meter is "label  count  bar  pct" sized to w.
func meter(s *Styles, st lipgloss.Style, label string, n, total, w int) string {
	frac := 0.0
	if total > 0 {
		frac = float64(n) / float64(total)
	}
	num := commas(n)
	bw := max(4, w-10-8-6)
	return s.Muted.Render(pad(label, 10, false)) + pad(num, 8, true) + " " + st.Render(bar(frac, bw)) + " " +
		s.Muted.Render(pad(fmt.Sprintf("%.0f%%", 100*frac), 4, true))
}

func kvLine(s *Styles, k, v string) string { return s.Muted.Render(pad(k, 13, false)) + " " + v }

func (v *dashboardView) proxiesCard(m *Model, w int) string {
	s := m.styles
	st := v.data.stats
	lines := []string{
		s.Bold.Render(commas(st.Proxies)) + s.Muted.Render(" proxies in the database"),
		meter(s, s.Good, "alive", st.ByStatus[db.StatusAlive], st.Proxies, w),
		meter(s, s.Bad, "dead", st.ByStatus[db.StatusDead], st.Proxies, w),
		meter(s, s.Muted, "unknown", st.ByStatus[db.StatusUnknown], st.Proxies, w),
		kvLine(s, "measured", fmt.Sprintf("%s (%s timed)", commas(st.Measured), commas(st.Timed))),
		kvLine(s, "listed", s.Warn.Render(commas(st.Listed))+s.Muted.Render(" on reputation lists")),
	}
	return strings.Join(lines, "\n")
}

func (v *dashboardView) latencyCard(m *Model, w int) string {
	s := m.styles
	st := v.data.stats
	alive := st.ByStatus[db.StatusAlive]
	l := st.Latency
	lines := []string{
		"best " + s.ms(st.BestMS).Render(msUnit(st.BestMS)) + s.Muted.Render("  ·  ") + "median " + s.ms(st.MedianMS).Render(msUnit(st.MedianMS)),
		meter(s, s.Good, "< 100ms", l.Under100, alive, w),
		meter(s, s.Good, "≤ 1s", l.To1s, alive, w),
		meter(s, s.Warn, "1-5s", l.To5s, alive, w),
		meter(s, s.Bad, "5-10s", l.To10s, alive, w),
		meter(s, s.Bad, "> 10s", l.Over10s, alive, w),
		meter(s, s.Muted, "untimed", l.Untimed, alive, w),
	}
	return strings.Join(lines, "\n")
}

func (v *dashboardView) schemesCard(m *Model, w int) string {
	s := m.styles
	st := v.data.stats
	var lines []string
	for _, sc := range proxy.AllSchemes {
		if n := st.ByScheme[string(sc)]; n > 0 {
			lines = append(lines, meter(s, s.Accent, string(sc), n, st.Proxies, w))
		}
	}
	var srcs []string
	for k, n := range st.BySource {
		if k == "" {
			k = "(none)"
		}
		srcs = append(srcs, fmt.Sprintf("%s %s", k, commas(n)))
	}
	sort.Strings(srcs)
	if len(srcs) > 0 {
		lines = append(lines, kvLine(s, "sources", truncate(strings.Join(srcs, ", "), max(10, w-14))))
	}
	if len(lines) == 0 {
		return s.Muted.Render("no proxies yet")
	}
	return strings.Join(lines, "\n")
}

func (v *dashboardView) factsCard(m *Model, _ int) string {
	s := m.styles
	st := v.data.stats
	lines := []string{
		kvLine(s, "measurements", commas(st.Measurements)+s.Muted.Render(" (last "+ago(st.LastMeasurement)+" ago)")),
		kvLine(s, "reputation", commas(st.Reputation)+s.Muted.Render(" verdicts")),
		kvLine(s, "usage", commas(st.Usage)+s.Muted.Render(" proxied commands")),
		kvLine(s, "tags", commas(st.Tags)+s.Muted.Render(" tag rows")),
		kvLine(s, "scan runs", commas(st.ScanRuns)),
		kvLine(s, "last check", ago(st.LastChecked)+s.Muted.Render(" ago")),
		kvLine(s, "schema", fmt.Sprintf("v%d", st.SchemaVersion)),
	}
	return strings.Join(lines, "\n")
}

func (v *dashboardView) tagsCard(m *Model, w int) string {
	s := m.styles
	tags := append([]db.TagCount(nil), v.data.tags...)
	if len(tags) == 0 {
		return s.Muted.Render("no tags yet: scans and uses add them\n(R in the Tags tab refreshes them now)")
	}
	sort.SliceStable(tags, func(i, j int) bool { return tags[i].Count > tags[j].Count })
	var lines []string
	for i, t := range tags {
		if i == 8 {
			lines = append(lines, s.Muted.Render(fmt.Sprintf("… %d more in the Tags tab", len(tags)-8)))
			break
		}
		label := t.Tag
		if t.Scope != "" {
			label += ":" + t.Scope
		}
		kind := s.Warn.Render("manual")
		if score.IsHeuristic(t.Tag) {
			kind = s.Muted.Render("heuristic")
		}
		lines = append(lines, s.tagStyle(label).Render(pad(label, max(8, w-20), false))+pad(commas(t.Count), 7, true)+"  "+kind)
	}
	return strings.Join(lines, "\n")
}

func (v *dashboardView) scansCard(m *Model, _ int) string {
	s := m.styles
	if len(v.data.runs) == 0 {
		return s.Muted.Render("no scans yet: px scan full (or quick)")
	}
	var lines []string
	for _, r := range v.data.runs {
		pct := 100 * float64(r.Done) / float64(max(r.Total, 1))
		lines = append(lines, fmt.Sprintf("%s %s %s %s  %s", s.Muted.Render(pad(fmt.Sprintf("#%d", r.ID), 5, false)),
			pad(r.Mode, 10, false), s.scanState(r.State).Render(pad(r.State, 8, false)),
			pad(fmt.Sprintf("%.0f%%", pct), 4, true), s.Muted.Render(ago(r.StartedAt)+" ago")))
	}
	return strings.Join(lines, "\n")
}

func (v *dashboardView) sessionCard(m *Model, _ int) string {
	s := m.styles
	se := v.data.session
	if !se.active {
		return s.Muted.Render("○ no proxy in this shell") + "\n" + s.Faint.Render("px on routes a whole shell session")
	}
	lines := []string{s.Good.Render("● proxy on") + "  " + s.Accent2.Render(dash(se.proxyURL))}
	if p := se.proxy; p != nil {
		lines = append(lines, kvLine(s, "status", s.status(p.Status).Render(p.Status)+"  last "+s.ms(p.LastMS).Render(msUnit(p.LastMS))+
			"  "+s.Info.Render(dash(p.Country))+"  #"+fmt.Sprint(p.ID)))
	} else if se.proxyURL != "" {
		lines = append(lines, s.Muted.Render("not in the database"))
	}
	if se.sessionID != "" {
		lines = append(lines, kvLine(s, "session", se.sessionID+s.Muted.Render(fmt.Sprintf(" · %d commands logged", se.uses))))
	}
	return strings.Join(lines, "\n")
}

func (v *dashboardView) usageCard(m *Model, w int) string {
	s := m.styles
	if len(v.data.cmds) == 0 {
		return s.Muted.Render("nothing routed this week (px <command>)")
	}
	var lines []string
	for _, g := range v.data.cmds {
		r := g.Rate()
		lines = append(lines, s.Accent2.Render(pad(g.Key[0], max(8, w-22), false))+pad(commas(g.Uses), 7, true)+
			s.Muted.Render(" uses ")+s.rate(r).Render(pad(pctText(r), 4, true)))
	}
	return strings.Join(lines, "\n")
}

func (v *dashboardView) status(m *Model) []string {
	if v.data == nil {
		return []string{"loading…"}
	}
	st := v.data.stats
	return []string{commas(st.Proxies) + " proxies", commas(st.ByStatus[db.StatusAlive]) + " alive",
		"db " + paths.Pretty(m.d.Path())}
}

func (v *dashboardView) help() []key.Binding {
	return []key.Binding{bind("↑/↓ pgup/pgdn", "scroll", "up")}
}

func (v *dashboardView) hints() []hint {
	return []hint{{"1-7", "tabs"}, {"↑↓", "scroll"}, {"ctrl+r", "reload"}}
}
