package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

// groupCycle is what 'g' walks through: the list, then stats by each key.
var groupCycle = []string{"", "proxy", "cmd", "err", "day", "hour", "week", "month", "mode", "session", "tty", "cwd"}

var (
	resultCycle    = []string{"", "ok", "fail", "proxy"}
	modeCycle      = []string{"", db.ModeRoute, db.ModeShell, db.ModeRelay}
	statsSortCycle = []string{"uses", "last", "fail", "rate", "key"}
)

type historyMsg struct {
	seq    int
	rows   []db.Usage
	total  int
	groups []db.UsageGroup
	err    error
}

// historyView is the usage log (px history) and its aggregates (px stats).
type historyView struct {
	baseView
	tbl    table
	rows   []db.Usage
	groups []db.UsageGroup
	total  int
	page   int
	seq    int
	loaded bool
	err    error

	group     string // "" = the list, else a db.UsageGroupKeys key
	statsSort string
	result    string // "", ok, fail, proxy or an err_class
	mode      string
	cmd       string
	session   string
	query     string

	search     textinput.Model
	searching  bool
	prevSearch string
}

func newHistoryView(m *Model) *historyView {
	v := &historyView{statsSort: "uses"}
	v.search = newInput(m, "/", "command line, cwd or proxy")
	v.setCols()
	return v
}

func (v *historyView) name() string  { return TabHistory }
func (v *historyView) title() string { return "History" }
func (v *historyView) about() string {
	return "The usage log, newest first: every command run through a proxy (`px <cmd>`, the shell hooks while " +
		"`px on` is active, `px shell`). `g` switches to aggregated statistics grouped by proxy, command, error " +
		"class, day, hour, ... like `px stats --by KEY`; Enter on a group narrows the log to it, Enter on a log row " +
		"opens its proxy."
}

func (v *historyView) capturing() bool { return v.searching }

func (v *historyView) filter() db.UsageFilter {
	return db.UsageFilter{Cmd: v.cmd, Mode: v.mode, ErrClass: v.result, SessionID: v.session, Search: v.query}
}

func (v *historyView) setCols() {
	if v.group == "" {
		v.tbl.setColumns([]column{{title: "TIME"}, {title: "MODE"}, {title: "PROXY", max: 34}, {title: "EXIT", right: true},
			{title: "TOOK", right: true}, {title: "MS", right: true}, {title: "ERROR"}, {title: "COMMAND", flex: true, min: 12}})
		v.tbl.empty = "no usage recorded (yet, or for these filters)"
		return
	}
	v.tbl.setColumns([]column{{title: strings.ToUpper(v.group), max: 40, flex: true, min: 12}, {title: "USES", right: true},
		{title: "OK", right: true}, {title: "FAIL", right: true}, {title: "PROXY ERR", right: true}, {title: "RATE", right: true},
		{title: "AVG MS", right: true}, {title: "AVG TOOK", right: true}, {title: "LAST", right: true}, {title: "ERRORS", flex: true}})
	v.tbl.empty = "no usage recorded for these filters"
}

func errClasses(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func (v *historyView) rebuild(m *Model) {
	s := m.styles
	var rows [][]cell
	if v.group == "" {
		for _, u := range v.rows {
			code, cc := "…", s.Info
			if u.ExitCode != nil {
				code, cc = strconv.Itoa(*u.ExitCode), s.Good
				if *u.ExitCode != 0 {
					cc = s.Bad
				}
			}
			errc := s.Muted
			if db.IsProxyErr(u.ErrClass) {
				errc = s.Bad
			} else if u.ErrClass != "" {
				errc = s.Warn
			}
			cmdline := strings.Join(u.Argv, " ")
			if len(u.Argv) > 0 && u.Cmd != "" {
				cmdline = strings.Join(append([]string{u.Cmd}, u.Argv[1:]...), " ")
			}
			rows = append(rows, []cell{{tsShort(u.TSStart), s.Muted}, plainCell(u.Mode), {dash(u.ProxyURL), s.Accent2},
				{code, cc}, plainCell(durText(u.DurationMS)), {msText(u.MS), s.ms(u.MS)}, {dash(u.ErrClass), errc},
				{cmdline, s.Text}})
		}
	} else {
		for _, g := range v.groups {
			r := g.Rate()
			avg := "-"
			if g.AvgMS > 0 {
				avg = strconv.Itoa(int(g.AvgMS + 0.5))
			}
			rows = append(rows, []cell{{g.Key[0], s.Accent2}, plainCell(commas(g.Uses)), {commas(g.OK), s.Good},
				{commas(g.Fail), s.Bad}, {commas(g.ProxyErr), s.Warn}, {pctText(r), s.rate(r)},
				{avg, s.ms(int(g.AvgMS))}, plainCell(durText(int64(g.AvgDurMS))), {ago(g.Last), s.Muted},
				{errClasses(g.ErrClasses), s.Muted}})
		}
	}
	v.tbl.setRows(rows)
}

func (v *historyView) resize(m *Model, w, h int) {
	v.w, v.h = w, h
	styleInput(m, &v.search)
	v.search.Width = max(10, w-4)
	v.tbl.setHeight(h)
	v.rebuild(m)
}

func (v *historyView) load(m *Model) tea.Cmd {
	v.seq++
	seq := v.seq
	f := v.filter()
	d, ctx := m.d, m.ctx
	if v.group != "" {
		by, order := v.group, v.statsSort
		return func() tea.Msg {
			gs, err := d.UsageStats(ctx, []string{by}, f, order, 0)
			return historyMsg{seq: seq, groups: gs, err: err}
		}
	}
	ps := m.pageSize()
	f.Limit, f.Offset = ps, v.page*ps
	return func() tea.Msg {
		rows, err := d.ListUsage(ctx, f)
		if err != nil {
			return historyMsg{seq: seq, err: err}
		}
		n, err := d.CountUsage(ctx, f)
		return historyMsg{seq: seq, rows: rows, total: n, err: err}
	}
}

func (v *historyView) refresh(m *Model) tea.Cmd { return v.load(m) }

// restart reloads from the first row after the filters or the grouping
// changed. The old rows go at once: they no longer match the columns.
func (v *historyView) restart(m *Model) tea.Cmd {
	v.page = 0
	v.tbl.reset()
	v.setCols()
	v.rows, v.groups, v.total = nil, nil, 0
	v.rebuild(m)
	return v.load(m)
}

func (v *historyView) update(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case historyMsg:
		if msg.seq != v.seq {
			return nil
		}
		v.loaded = true
		v.err = msg.err
		if msg.err != nil {
			return nil
		}
		v.rows, v.groups, v.total = msg.rows, msg.groups, msg.total
		v.rebuild(m)
	default:
		if v.searching {
			var cmd tea.Cmd
			v.search, cmd = v.search.Update(msg)
			return cmd
		}
	}
	return nil
}

func (v *historyView) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	if v.searching {
		switch msg.String() {
		case "enter":
			v.searching = false
			v.search.Blur()
			return nil
		case "esc":
			v.searching = false
			v.search.Blur()
			if v.query != v.prevSearch {
				v.query = v.prevSearch
				return v.restart(m)
			}
			return nil
		}
		var cmd tea.Cmd
		v.search, cmd = v.search.Update(msg)
		if val := v.search.Value(); val != v.query {
			v.query = val
			return tea.Batch(cmd, v.restart(m))
		}
		return cmd
	}
	if navigate(&v.tbl, msg, keys.Up) {
		return nil
	}
	ps := m.pageSize()
	switch {
	case key.Matches(msg, keys.Enter):
		return v.enter(m)
	case key.Matches(msg, keys.Stats):
		v.group = cycle(groupCycle, v.group, 1)
		return v.restart(m)
	case key.Matches(msg, keys.Back):
		if v.group != "" {
			v.group = ""
			return v.restart(m)
		}
	case key.Matches(msg, keys.StatsSort):
		if v.group == "" {
			return m.notify(levelInfo, "the log is always newest first (g for stats)")
		}
		v.statsSort = cycle(statsSortCycle, v.statsSort, 1)
		return v.restart(m)
	case key.Matches(msg, keys.Result):
		v.result = cycle(resultCycle, v.result, 1)
		return v.restart(m)
	case key.Matches(msg, keys.Mode):
		v.mode = cycle(modeCycle, v.mode, 1)
		return v.restart(m)
	case key.Matches(msg, keys.Clear):
		v.result, v.mode, v.cmd, v.session, v.query = "", "", "", "", ""
		return tea.Batch(v.restart(m), m.notify(levelInfo, "filters cleared"))
	case key.Matches(msg, keys.Search):
		v.searching = true
		v.prevSearch = v.query
		v.search.SetValue(v.query)
		v.search.CursorEnd()
		return v.search.Focus()
	case key.Matches(msg, keys.NextPage):
		if v.group == "" && (v.page+1)*ps < v.total {
			v.page++
			v.tbl.reset()
			return v.load(m)
		}
	case key.Matches(msg, keys.PrevPage):
		if v.group == "" && v.page > 0 {
			v.page--
			v.tbl.reset()
			return v.load(m)
		}
	}
	return nil
}

// enter opens the proxy of a log row, or narrows the log to a group.
func (v *historyView) enter(m *Model) tea.Cmd {
	i := v.tbl.selected()
	if i < 0 || (v.group == "" && i >= len(v.rows)) || (v.group != "" && i >= len(v.groups)) {
		return nil
	}
	if v.group == "" {
		u := v.rows[i]
		if u.ProxyID <= 0 {
			return m.notify(levelWarn, "this command's proxy is not in the database")
		}
		return m.showProxy(u.ProxyID)
	}
	k := v.groups[i].Key[0]
	switch v.group {
	case "proxy":
		v.query = k
	case "cmd":
		v.cmd = k
	case "err":
		if k != "running" {
			v.result = k
		}
	case "mode":
		v.mode = k
	case "session":
		if k != "-" {
			v.session = k
		}
	default:
		return m.notify(levelInfo, "no drill-down for %s groups (esc: back to the log)", v.group)
	}
	v.group = ""
	return v.restart(m)
}

func (v *historyView) click(m *Model, _, y int) tea.Cmd {
	if v.searching {
		return nil
	}
	i := v.tbl.rowAt(y)
	if i < 0 {
		return nil
	}
	if i == v.tbl.cursor {
		return v.enter(m)
	}
	v.tbl.cursor = i
	v.tbl.clamp()
	return nil
}

func (v *historyView) scroll(m *Model, d int) tea.Cmd {
	v.tbl.move(d)
	return nil
}

func (v *historyView) render(m *Model) string {
	s := m.styles
	if v.err != nil {
		return s.Error.Render("  " + v.err.Error())
	}
	if !v.loaded {
		return s.Muted.Render("  loading the usage log…")
	}
	return v.tbl.view(s, v.w)
}

func (v *historyView) status(m *Model) []string {
	var segs []string
	ps := m.pageSize()
	switch {
	case !v.loaded:
		segs = append(segs, "loading…")
	case v.group != "":
		segs = append(segs, fmt.Sprintf("%d groups by %s", len(v.groups), v.group), "order:"+v.statsSort)
	case v.total > 0:
		seg := commas(v.page*ps+1) + "-" + commas(v.page*ps+len(v.rows)) + " of " + commas(v.total)
		if pages := (v.total + ps - 1) / ps; pages > 1 {
			seg += fmt.Sprintf(" (page %d/%d)", v.page+1, pages)
		}
		segs = append(segs, seg)
	default:
		segs = append(segs, "0 commands")
	}
	for _, f := range [][2]string{{"result", v.result}, {"mode", v.mode}, {"cmd", v.cmd}, {"session", v.session}} {
		if f[1] != "" {
			segs = append(segs, f[0]+":"+f[1])
		}
	}
	if v.query != "" {
		segs = append(segs, "/"+v.query)
	}
	return segs
}

func (v *historyView) footer(m *Model) string {
	if v.searching {
		return v.search.View() + m.styles.Faint.Render("   enter keep · esc cancel")
	}
	return ""
}

func (v *historyView) help() []key.Binding {
	return []key.Binding{
		bind("enter", "log: open the proxy · stats: narrow the log to the group", "enter"),
		keys.Stats, keys.StatsSort, bind("esc", "back from the stats to the log", "esc"),
		keys.Result, keys.Mode, keys.Search, keys.Clear, keys.PrevPage, keys.NextPage,
	}
}

func (v *historyView) hints() []hint {
	if v.group != "" {
		return []hint{{"g", "group by"}, {"s", "order"}, {"enter", "narrow"}, {"esc", "log"}, {"f/m", "filter"}}
	}
	return []hint{{"↑↓", "move"}, {"enter", "proxy"}, {"g", "stats"}, {"/", "search"}, {"f/m", "filter"}, {"[ ]", "page"}}
}
