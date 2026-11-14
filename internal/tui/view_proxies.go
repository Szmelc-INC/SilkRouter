package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
)

// ---- columns (tui.columns) ----------------------------------------------------

// proxyCol is one column the proxy table can show.
type proxyCol struct {
	name string
	col  column
	cell func(s *Styles, p *db.Proxy) cell
}

func anonShort(a string) string {
	switch a {
	case "anonymous":
		return "anon"
	case "transparent":
		return "transp"
	case "", "unknown":
		return "-"
	}
	return a
}

func scoreCell(s *Styles, f *float64) cell { return cell{fptr(f), s.score(f)} }

var proxyColDefs = []proxyCol{
	{"id", column{title: "ID", right: true}, func(s *Styles, p *db.Proxy) cell {
		return cell{strconv.FormatInt(p.ID, 10), s.Muted}
	}},
	{"proxy", column{title: "PROXY", max: 46}, func(s *Styles, p *db.Proxy) cell { return cell{p.Key(), s.Accent2} }},
	{"scheme", column{title: "SCHEME"}, func(s *Styles, p *db.Proxy) cell { return cell{p.Scheme, s.Text} }},
	{"host", column{title: "HOST", max: 40}, func(s *Styles, p *db.Proxy) cell { return cell{p.Host, s.Text} }},
	{"port", column{title: "PORT", right: true}, func(s *Styles, p *db.Proxy) cell { return cell{strconv.Itoa(p.Port), s.Text} }},
	{"status", column{title: "STATUS"}, func(s *Styles, p *db.Proxy) cell { return cell{p.Status, s.status(p.Status)} }},
	{"last", column{title: "LAST", right: true}, func(s *Styles, p *db.Proxy) cell { return cell{msText(p.LastMS), s.ms(p.LastMS)} }},
	{"best", column{title: "BEST", right: true}, func(s *Styles, p *db.Proxy) cell { return cell{msText(p.BestMS), s.ms(p.BestMS)} }},
	{"avg", column{title: "AVG", right: true}, func(s *Styles, p *db.Proxy) cell {
		if p.AvgMS <= 0 {
			return cell{"-", s.Muted}
		}
		return cell{strconv.Itoa(int(p.AvgMS + 0.5)), s.ms(int(p.AvgMS))}
	}},
	{"ok", column{title: "OK", right: true}, func(s *Styles, p *db.Proxy) cell { return cell{strconv.Itoa(p.OKCount), s.Text} }},
	{"fail", column{title: "FAIL", right: true}, func(s *Styles, p *db.Proxy) cell { return cell{strconv.Itoa(p.FailCount), s.Text} }},
	{"streak", column{title: "STREAK", right: true}, func(s *Styles, p *db.Proxy) cell {
		switch {
		case p.OKStreak > 0:
			return cell{"+" + strconv.Itoa(p.OKStreak), s.Good}
		case p.FailStreak > 0:
			return cell{"-" + strconv.Itoa(p.FailStreak), s.Bad}
		}
		return cell{"0", s.Muted}
	}},
	{"rate", column{title: "RATE", right: true}, func(s *Styles, p *db.Proxy) cell {
		r := p.SuccessRate()
		return cell{pctText(r), s.rate(r)}
	}},
	{"checked", column{title: "CHECKED", right: true}, func(s *Styles, p *db.Proxy) cell { return cell{ago(p.CheckedAt), s.Muted} }},
	{"cc", column{title: "CC"}, func(s *Styles, p *db.Proxy) cell { return cell{dash(p.Country), s.Info} }},
	{"anon", column{title: "ANON"}, func(s *Styles, p *db.Proxy) cell { return cell{anonShort(p.Anonymity), s.anon(p.Anonymity)} }},
	{"bl", column{title: "BL", right: true}, func(s *Styles, p *db.Proxy) cell {
		if p.ListedCount > 0 {
			return cell{strconv.Itoa(p.ListedCount), s.Bad}
		}
		return cell{"0", s.Muted}
	}},
	{"rank", column{title: "RANK", right: true}, func(s *Styles, p *db.Proxy) cell {
		if p.Rank != nil && *p.Rank <= 10 {
			return cell{iptr(p.Rank), s.Accent}
		}
		return cell{iptr(p.Rank), s.Text}
	}},
	{"speed", column{title: "SPD", right: true}, func(s *Styles, p *db.Proxy) cell { return scoreCell(s, p.SpeedScore) }},
	{"clean", column{title: "CLN", right: true}, func(s *Styles, p *db.Proxy) cell { return scoreCell(s, p.CleanScore) }},
	{"stable", column{title: "STB", right: true}, func(s *Styles, p *db.Proxy) cell { return scoreCell(s, p.StableScore) }},
	{"uses", column{title: "USES", right: true}, func(s *Styles, p *db.Proxy) cell { return cell{strconv.Itoa(p.UseCount), s.Text} }},
	{"used", column{title: "USED", right: true}, func(s *Styles, p *db.Proxy) cell { return cell{ago(p.LastUsedAt), s.Muted} }},
	{"exit", column{title: "EXIT IP"}, func(s *Styles, p *db.Proxy) cell { return cell{dash(p.ExitIP), s.Text} }},
	{"org", column{title: "ORG", max: 24}, func(s *Styles, p *db.Proxy) cell { return cell{dash(p.Org), s.Muted} }},
	{"source", column{title: "SOURCE", max: 16}, func(s *Styles, p *db.Proxy) cell { return cell{p.Source, s.Muted} }},
	{"tags", column{title: "TAGS", flex: true, min: 8}, func(s *Styles, p *db.Proxy) cell {
		return cell{strings.Join(p.Tags, " "), s.Tag}
	}},
}

// ProxyColumns lists the column names tui.columns accepts.
func ProxyColumns() []string {
	out := make([]string, len(proxyColDefs))
	for i, c := range proxyColDefs {
		out[i] = c.name
	}
	return out
}

func proxyColByName(name string) (proxyCol, bool) {
	for _, c := range proxyColDefs {
		if c.name == name {
			return c, true
		}
	}
	return proxyCol{}, false
}

// ---- filter ---------------------------------------------------------------------

var schemeCycle = []string{"all", "http", "https", "socks", "socks4", "socks4a", "socks5"}

// sortCycle is what 's' walks through (':sort' takes any db sort key).
var sortCycle = []string{"rank", "last_ms", "avg_ms", "best_ms", "success_rate", "ok_streak", "checked_at",
	"speed_score", "clean_score", "stable_score", "listed", "use_count", "last_used_at", "country", "anonymity", "host", "id"}

// descNatural marks the sort keys whose natural direction is descending
// (see internal/db/filter.go), for the arrow and ':sort KEY asc|desc'.
var descNatural = map[string]bool{
	"speed_score": true, "clean_score": true, "ok_streak": true, "fail_streak": true, "ok_count": true,
	"fail_count": true, "success_rate": true, "checked_at": true, "added_at": true, "stable_score": true,
	"use_count": true, "use_rate": true, "last_used_at": true, "full_scan_at": true,
}

var anonValues = []string{"elite", "anonymous", "transparent", "unknown"}

// filterKeys are the ':filter KEY=VALUE' keys.
var filterKeys = []string{"status", "scheme", "tag", "max_ms", "min_ms", "country", "listed_max", "min_streak",
	"source", "anon", "https", "dead", "search"}

// proxyFilter is the proxy table's selection, turned into a db.Filter.
type proxyFilter struct {
	status    string // StatusFilters
	showDead  bool   // with status "all"
	scheme    string // schemeCycle
	sort      string
	reverse   bool
	search    string
	tag       string
	tagScope  string
	maxMS     int
	minMS     int
	countries []string
	listedMax int // -1 = any
	minStreak int
	source    string
	anon      []string
	https     bool
}

func (f proxyFilter) dbFilter() db.Filter {
	df := db.Filter{
		Sort: f.sort, Reverse: f.reverse, Search: f.search, Tag: f.tag, TagScope: f.tagScope,
		MinMS: f.minMS, MaxMS: f.maxMS, Countries: f.countries, MinOKStreak: f.minStreak, Source: f.source,
		Anonymity: f.anon, HTTPSOnly: f.https, WithTags: true,
	}
	switch f.status {
	case "", "all":
		if !f.showDead {
			df.Status = []string{db.StatusAlive, db.StatusUnknown}
		}
	default:
		df.Status = []string{f.status}
	}
	if f.scheme != "" && f.scheme != "all" {
		df.Schemes = db.ExpandSchemes([]string{f.scheme}, false)
	}
	if f.listedMax >= 0 {
		n := f.listedMax
		df.ListedMax = &n
	}
	return df
}

// cleared drops every filter and the search (sort and dead rows stay).
func (f proxyFilter) cleared() proxyFilter {
	return proxyFilter{status: "all", showDead: f.showDead, scheme: "all", sort: f.sort, reverse: f.reverse, listedMax: -1}
}

func (f proxyFilter) arrow() string {
	if descNatural[f.sort] != f.reverse {
		return "↓"
	}
	return "↑"
}

func (f proxyFilter) segments() []string {
	st := "status:" + f.status
	if (f.status == "all" || f.status == "") && !f.showDead {
		st += " (dead hidden)"
	}
	out := []string{st}
	if f.scheme != "" && f.scheme != "all" {
		out = append(out, "scheme:"+f.scheme)
	}
	out = append(out, "sort:"+f.sort+" "+f.arrow())
	if f.tag != "" {
		t := f.tag
		if f.tagScope != "" {
			t += ":" + f.tagScope
		}
		out = append(out, "tag:"+t)
	}
	if f.maxMS > 0 {
		out = append(out, "≤"+strconv.Itoa(f.maxMS)+"ms")
	}
	if f.minMS > 0 {
		out = append(out, "≥"+strconv.Itoa(f.minMS)+"ms")
	}
	if len(f.countries) > 0 {
		out = append(out, "cc:"+strings.Join(f.countries, ","))
	}
	if f.listedMax >= 0 {
		out = append(out, "bl≤"+strconv.Itoa(f.listedMax))
	}
	if f.minStreak > 0 {
		out = append(out, "streak≥"+strconv.Itoa(f.minStreak))
	}
	if f.source != "" {
		out = append(out, "source:"+f.source)
	}
	if len(f.anon) > 0 {
		out = append(out, "anon:"+strings.Join(f.anon, ","))
	}
	if f.https {
		out = append(out, "https")
	}
	if f.search != "" {
		out = append(out, "/"+f.search)
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// apply parses ':filter KEY=VALUE ...'; no argument (or "clear") resets.
func (f proxyFilter) apply(args []string) (proxyFilter, error) {
	if len(args) == 0 || (len(args) == 1 && contains([]string{"clear", "reset", "none", "off"}, args[0])) {
		return f.cleared(), nil
	}
	for _, a := range args {
		k, val, ok := strings.Cut(a, "=")
		if !ok {
			return f, fmt.Errorf("filter %q: want KEY=VALUE (keys: %s)", a, strings.Join(filterKeys, ", "))
		}
		k, val = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(val)
		switch k {
		case "status":
			if val == "" || val == "any" {
				val = "all"
			}
			if !contains(StatusFilters, val) {
				return f, fmt.Errorf("status %q (want one of: %s)", val, strings.Join(StatusFilters, ", "))
			}
			f.status = val
		case "scheme":
			val = strings.ToLower(val)
			if val == "" || val == "any" {
				val = "all"
			}
			if !contains(schemeCycle, val) {
				return f, fmt.Errorf("scheme %q (want one of: %s)", val, strings.Join(schemeCycle, ", "))
			}
			f.scheme = val
		case "tag":
			f.tag, f.tagScope, _ = strings.Cut(val, ":")
		case "max_ms", "min_ms", "listed_max", "min_streak":
			n := 0
			if k == "listed_max" {
				n = -1
			}
			if val != "" {
				var err error
				if n, err = strconv.Atoi(val); err != nil || n < -1 {
					return f, fmt.Errorf("%s: %q is not a number", k, val)
				}
			}
			switch k {
			case "max_ms":
				f.maxMS = max(0, n)
			case "min_ms":
				f.minMS = max(0, n)
			case "listed_max":
				f.listedMax = n
			case "min_streak":
				f.minStreak = max(0, n)
			}
		case "country", "cc":
			f.countries = splitList(strings.ToUpper(val))
		case "source":
			f.source = val
		case "anon", "anonymity":
			vals := splitList(strings.ToLower(val))
			for _, v := range vals {
				if !contains(anonValues, v) {
					return f, fmt.Errorf("anon %q (want: %s)", v, strings.Join(anonValues, ", "))
				}
			}
			f.anon = vals
		case "https", "dead":
			b := false
			if val != "" {
				var err error
				if b, err = strconv.ParseBool(val); err != nil {
					return f, fmt.Errorf("%s: %q is not true/false", k, val)
				}
			}
			if k == "https" {
				f.https = b
			} else {
				f.showDead = b
			}
		case "search":
			f.search = val
		default:
			return f, fmt.Errorf("unknown filter %q (keys: %s)", k, strings.Join(filterKeys, ", "))
		}
	}
	return f, nil
}

// withSort applies ':sort KEY [asc|desc|rev]'.
func (f proxyFilter) withSort(k, dir string) (proxyFilter, error) {
	if !contains(db.SortKeys(), k) {
		return f, fmt.Errorf("unknown sort key %q (want one of: %s)", k, strings.Join(db.SortKeys(), ", "))
	}
	f.sort = k
	switch strings.ToLower(dir) {
	case "":
		f.reverse = false
	case "asc", "up":
		f.reverse = descNatural[k]
	case "desc", "down":
		f.reverse = !descNatural[k]
	case "rev", "reverse", "-r":
		f.reverse = true
	default:
		return f, fmt.Errorf("sort direction %q (want asc, desc or rev)", dir)
	}
	return f, nil
}

// ---- view -------------------------------------------------------------------------

type proxiesMsg struct {
	seq   int
	rows  []*db.Proxy
	total int
	err   error
}

type proxiesView struct {
	baseView
	f       proxyFilter
	cols    []proxyCol
	tbl     table
	rows    []*db.Proxy
	total   int
	page    int
	seq     int
	loading bool
	loaded  bool
	err     error
	keepID  int64 // keep the cursor on this proxy after a reload

	search     textinput.Model
	searching  bool
	prevSearch string

	prompt    textinput.Model
	promptAct string // "" | "tag" | "untag"
	promptID  int64
	promptKey string

	detail *detailView
}

func newProxiesView(m *Model, o Options) *proxiesView {
	v := &proxiesView{f: proxyFilter{status: o.Status, showDead: o.ShowDead, scheme: "all", sort: o.Sort,
		reverse: o.Reverse, search: o.Search, listedMax: -1}}
	v.search = newInput(m, "/", "host, URL, exit IP, country, org or source")
	v.prompt = newInput(m, "", "")
	v.tbl.empty = "no proxies match (X clears the filters, D shows dead rows)"
	v.setColumns(m)
	return v
}

func (v *proxiesView) name() string  { return TabProxies }
func (v *proxiesView) title() string { return "Proxies" }
func (v *proxiesView) about() string {
	return "Every proxy in the database, colored like `px proxies`. The table loads `tui.page_size` rows at a time " +
		"(`[` / `]` page); `/` searches host, URL, exit IP, country, org and source as you type. Enter opens the " +
		"details (fields, scores, recent measurements, reputation verdicts, tags, usage). Actions work on the " +
		"selected row or the open details; checks run in the background and are recorded as `probe` measurements."
}

func (v *proxiesView) capturing() bool { return v.searching || v.promptAct != "" }

// setColumns applies tui.columns and returns the names it does not know.
func (v *proxiesView) setColumns(m *Model) (unknown []string) {
	names := m.cfg.TUI.Columns
	if len(names) == 0 {
		names = config.Default().TUI.Columns
	}
	v.cols = nil
	for _, n := range names {
		if c, ok := proxyColByName(strings.ToLower(strings.TrimSpace(n))); ok {
			v.cols = append(v.cols, c)
		} else {
			unknown = append(unknown, n)
		}
	}
	if len(v.cols) == 0 {
		for _, n := range config.Default().TUI.Columns {
			c, _ := proxyColByName(n)
			v.cols = append(v.cols, c)
		}
	}
	cols := make([]column, len(v.cols))
	for i, c := range v.cols {
		cols[i] = c.col
	}
	v.tbl.setColumns(cols)
	v.rebuild(m)
	return unknown
}

// columnsWarning is the toast for unknown tui.columns entries.
func columnsWarning(unknown []string) string {
	return fmt.Sprintf("tui.columns: unknown %s (known: %s)", strings.Join(unknown, ", "), strings.Join(ProxyColumns(), ", "))
}

func (v *proxiesView) rebuild(m *Model) {
	rows := make([][]cell, len(v.rows))
	for i, p := range v.rows {
		r := make([]cell, len(v.cols))
		for j, c := range v.cols {
			r[j] = c.cell(m.styles, p)
		}
		rows[i] = r
	}
	v.tbl.setRows(rows)
	if v.keepID != 0 {
		for i, p := range v.rows {
			if p.ID == v.keepID {
				v.tbl.cursor = i
				v.tbl.clamp()
				break
			}
		}
		v.keepID = 0
	}
}

func (v *proxiesView) resize(m *Model, w, h int) {
	v.w, v.h = w, h
	styleInput(m, &v.search)
	styleInput(m, &v.prompt)
	v.search.Width = max(10, w-4)
	v.prompt.Width = max(10, w-lipgloss.Width(v.prompt.Prompt)-2)
	v.tbl.setHeight(h)
	v.rebuild(m)
	if v.detail != nil {
		v.detail.resize(m, w, h)
	}
}

// load queries the current page (and the total) in the background.
func (v *proxiesView) load(m *Model) tea.Cmd {
	v.seq++
	seq := v.seq
	v.loading = true
	f := v.f.dbFilter()
	ps := m.pageSize()
	f.Limit, f.Offset = ps, v.page*ps
	d, ctx := m.d, m.ctx
	return func() tea.Msg {
		rows, err := d.ListProxies(ctx, f)
		if err != nil {
			return proxiesMsg{seq: seq, err: err}
		}
		cf := f
		cf.Limit, cf.Offset, cf.WithTags = 0, 0, false
		n, err := d.CountProxies(ctx, cf)
		return proxiesMsg{seq: seq, rows: rows, total: n, err: err}
	}
}

// reloadFiltered starts over at the first row (the selection changed).
func (v *proxiesView) reloadFiltered(m *Model) tea.Cmd {
	v.page = 0
	v.keepID = 0
	v.tbl.reset()
	return v.load(m)
}

func (v *proxiesView) refresh(m *Model) tea.Cmd {
	if i := v.tbl.selected(); i >= 0 && i < len(v.rows) {
		v.keepID = v.rows[i].ID
	}
	cmds := []tea.Cmd{v.load(m)}
	if v.detail != nil {
		cmds = append(cmds, v.detail.load(m))
	}
	return tea.Batch(cmds...)
}

// current is the proxy actions apply to: the open details, else the
// selected row.
func (v *proxiesView) current() *db.Proxy {
	if v.detail != nil {
		if v.detail.data != nil {
			return v.detail.data.p
		}
		return nil
	}
	if i := v.tbl.selected(); i >= 0 && i < len(v.rows) {
		return v.rows[i]
	}
	return nil
}

func (v *proxiesView) openDetail(m *Model, id int64) tea.Cmd {
	v.detail = newDetailView(id)
	v.detail.resize(m, v.w, v.h)
	return v.detail.load(m)
}

func (v *proxiesView) replace(m *Model, p *db.Proxy) {
	for i, r := range v.rows {
		if r.ID == p.ID {
			v.rows[i] = p
			v.rebuild(m)
			return
		}
	}
}

func (v *proxiesView) isActive(m *Model) bool { return m.activeView() == view(v) }

func (v *proxiesView) update(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case proxiesMsg:
		if msg.seq != v.seq {
			return nil // a newer query is on its way
		}
		v.loading, v.loaded = false, true
		if msg.err != nil {
			v.err = msg.err
			return nil
		}
		v.err = nil
		v.rows, v.total = msg.rows, msg.total
		if ps := m.pageSize(); len(v.rows) == 0 && v.page > 0 && v.total > 0 {
			v.page = (v.total - 1) / ps // the page vanished: show the last one
			return v.load(m)
		}
		v.rebuild(m)
	case detailMsg:
		if v.detail != nil {
			v.detail.apply(m, msg)
		}
	case checkDoneMsg:
		if msg.proxy != nil {
			v.replace(m, msg.proxy)
		}
		if v.detail != nil && v.detail.id == msg.id {
			return v.detail.load(m)
		}
	case tagsChangedMsg:
		if msg.err == nil && v.isActive(m) {
			return v.refresh(m)
		}
	case rankDoneMsg, retagDoneMsg:
		if v.isActive(m) {
			return v.refresh(m)
		}
	default:
		var cmd tea.Cmd
		switch {
		case v.searching:
			v.search, cmd = v.search.Update(msg)
		case v.promptAct != "":
			v.prompt, cmd = v.prompt.Update(msg)
		}
		return cmd
	}
	return nil
}

func (v *proxiesView) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch {
	case v.promptAct != "":
		return v.promptKeyPress(m, msg)
	case v.searching:
		return v.searchKey(m, msg)
	case v.detail != nil:
		if cmd, ok := v.detail.key(m, v, msg); ok {
			return cmd
		}
	default:
		if navigate(&v.tbl, msg, keys.Up) {
			return nil
		}
	}
	// Actions: on the details or the selected row.
	switch {
	case key.Matches(msg, keys.Tag):
		return v.startPrompt(m, "tag", v.current())
	case key.Matches(msg, keys.Untag):
		return v.startPrompt(m, "untag", v.current())
	case key.Matches(msg, keys.Block):
		return m.toggleBlocked(v.current())
	case key.Matches(msg, keys.Check):
		return m.checkProxy(v.current())
	case key.Matches(msg, keys.Copy):
		return m.copyURL(v.current())
	}
	if v.detail != nil {
		return nil
	}
	ps := m.pageSize()
	switch {
	case key.Matches(msg, keys.Enter):
		if p := v.current(); p != nil {
			return v.openDetail(m, p.ID)
		}
	case key.Matches(msg, keys.NextPage):
		if (v.page+1)*ps < v.total {
			v.page++
			v.tbl.reset()
			return v.load(m)
		}
		return m.notify(levelInfo, "last page")
	case key.Matches(msg, keys.PrevPage):
		if v.page > 0 {
			v.page--
			v.tbl.reset()
			return v.load(m)
		}
		return m.notify(levelInfo, "first page")
	case key.Matches(msg, keys.Search):
		v.searching = true
		v.prevSearch = v.f.search
		v.search.SetValue(v.f.search)
		v.search.CursorEnd()
		return v.search.Focus()
	case key.Matches(msg, keys.Sort), key.Matches(msg, keys.SortBack):
		step := 1
		if key.Matches(msg, keys.SortBack) {
			step = -1
		}
		v.f.sort = cycle(sortCycle, v.f.sort, step)
		v.f.reverse = false
		return v.reloadFiltered(m)
	case key.Matches(msg, keys.Reverse):
		v.f.reverse = !v.f.reverse
		return v.reloadFiltered(m)
	case key.Matches(msg, keys.Status):
		v.f.status = cycle(StatusFilters, v.f.status, 1)
		return v.reloadFiltered(m)
	case key.Matches(msg, keys.Scheme):
		v.f.scheme = cycle(schemeCycle, v.f.scheme, 1)
		return v.reloadFiltered(m)
	case key.Matches(msg, keys.Dead):
		v.f.showDead = !v.f.showDead
		state := "hidden"
		if v.f.showDead {
			state = "shown"
		}
		return tea.Batch(v.reloadFiltered(m), m.notify(levelInfo, "dead proxies %s (status all)", state))
	case key.Matches(msg, keys.Clear):
		v.f = v.f.cleared()
		return tea.Batch(v.reloadFiltered(m), m.notify(levelInfo, "filters cleared"))
	}
	return nil
}

func (v *proxiesView) searchKey(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		v.searching = false
		v.search.Blur()
		return nil
	case "esc":
		v.searching = false
		v.search.Blur()
		if v.f.search != v.prevSearch {
			v.f.search = v.prevSearch
			return v.reloadFiltered(m)
		}
		return nil
	}
	var cmd tea.Cmd
	v.search, cmd = v.search.Update(msg)
	if val := v.search.Value(); val != v.f.search {
		v.f.search = val
		return tea.Batch(cmd, v.reloadFiltered(m))
	}
	return cmd
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (v *proxiesView) startPrompt(m *Model, act string, p *db.Proxy) tea.Cmd {
	if p == nil {
		return m.notify(levelWarn, "no proxy selected")
	}
	v.prompt.SetValue("")
	v.prompt.ShowSuggestions = true
	switch act {
	case "tag":
		v.prompt.Prompt = " add tag(s) to " + p.Key() + " › "
		v.prompt.Placeholder = "TAG[:SCOPE] ... (blocked, pinned, work, ...)"
		v.prompt.SetSuggestions(dedupe(append([]string{"blocked", "pinned"}, m.tagLabels...)))
	case "untag":
		if len(p.Tags) == 0 {
			return m.notify(levelInfo, "%s has no tags", p.Key())
		}
		v.prompt.Prompt = " remove tag(s) from " + p.Key() + " › "
		v.prompt.Placeholder = strings.Join(p.Tags, " ")
		v.prompt.SetSuggestions(p.Tags)
		if len(p.Tags) == 1 {
			v.prompt.SetValue(p.Tags[0])
			v.prompt.CursorEnd()
		}
	}
	v.promptAct, v.promptID, v.promptKey = act, p.ID, p.Key()
	v.prompt.Width = max(10, v.w-lipgloss.Width(v.prompt.Prompt)-2)
	return v.prompt.Focus()
}

func (v *proxiesView) promptKeyPress(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		v.promptAct = ""
		v.prompt.Blur()
		return nil
	case "enter":
		act, id, k := v.promptAct, v.promptID, v.promptKey
		labels := strings.Fields(v.prompt.Value())
		v.promptAct = ""
		v.prompt.Blur()
		if len(labels) == 0 {
			return nil
		}
		if act == "tag" {
			return m.addTags(id, k, labels)
		}
		return m.removeTags(id, k, labels)
	}
	var cmd tea.Cmd
	v.prompt, cmd = v.prompt.Update(msg)
	return cmd
}

func (v *proxiesView) scroll(m *Model, d int) tea.Cmd {
	if v.detail != nil {
		v.detail.scroll(d)
		return nil
	}
	v.tbl.move(d)
	return nil
}

func (v *proxiesView) click(m *Model, _, y int) tea.Cmd {
	if v.detail != nil || v.capturing() {
		return nil
	}
	i := v.tbl.rowAt(y)
	if i < 0 {
		return nil
	}
	if i == v.tbl.cursor {
		return v.openDetail(m, v.rows[i].ID)
	}
	v.tbl.cursor = i
	v.tbl.clamp()
	return nil
}

func (v *proxiesView) render(m *Model) string {
	if v.detail != nil {
		return v.detail.render(m)
	}
	s := m.styles
	if v.err != nil && len(v.rows) == 0 {
		return s.Error.Render("  " + v.err.Error())
	}
	if !v.loaded {
		return s.Muted.Render("  loading proxies…")
	}
	return v.tbl.view(s, v.w)
}

func (v *proxiesView) status(m *Model) []string {
	if v.detail != nil {
		return v.detail.status(m)
	}
	var segs []string
	if v.err != nil {
		segs = append(segs, "error: "+v.err.Error())
	}
	ps := m.pageSize()
	switch {
	case !v.loaded:
		segs = append(segs, "loading…")
	case v.total > 0:
		from, to := v.page*ps+1, v.page*ps+len(v.rows)
		seg := commas(from) + "-" + commas(to) + " of " + commas(v.total)
		if pages := (v.total + ps - 1) / ps; pages > 1 {
			seg += fmt.Sprintf(" (page %d/%d)", v.page+1, pages)
		}
		segs = append(segs, seg)
	default:
		segs = append(segs, "0 proxies")
	}
	return append(segs, v.f.segments()...)
}

func (v *proxiesView) footer(m *Model) string {
	switch {
	case v.promptAct != "":
		return v.prompt.View()
	case v.searching:
		return v.search.View() + m.styles.Faint.Render("   enter keep · esc cancel")
	}
	return ""
}

func (v *proxiesView) help() []key.Binding {
	return []key.Binding{
		bind("enter", "open the details of the proxy", "enter"),
		keys.Search, keys.Sort, keys.SortBack, keys.Reverse, keys.Status, keys.Scheme, keys.Dead, keys.Clear,
		keys.PrevPage, keys.NextPage, keys.Tag, keys.Untag, keys.Block, keys.Check, keys.Copy,
		bind("esc", "close the details", "esc"),
		bind("r", "reload the details", "r"),
	}
}

func (v *proxiesView) hints() []hint {
	if v.detail != nil {
		return []hint{{"esc", "back"}, {"↑↓", "scroll"}, {"t/T", "tag"}, {"b", "block"}, {"c", "check"}, {"y", "copy"}}
	}
	return []hint{{"↑↓", "move"}, {"enter", "details"}, {"/", "search"}, {"s/r", "sort"}, {"a/p/D", "filter"},
		{"t/b/c/y", "tag/block/check/copy"}}
}
