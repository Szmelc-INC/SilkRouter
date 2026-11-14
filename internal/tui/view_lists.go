package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/reputation"
)

type listRow struct {
	meta  reputation.Meta
	desc  string
	fresh bool
}

type listsMsg struct {
	seq  int
	rows []listRow
	dir  string
	ttl  time.Duration
	err  error
}

// listsView shows the reputation lists scans match IPs against (px lists).
type listsView struct {
	baseView
	tbl    table
	rows   []listRow
	dir    string
	ttl    time.Duration
	seq    int
	loaded bool
	err    error
}

func newListsView() *listsView {
	v := &listsView{}
	v.tbl.setColumns([]column{{title: "LIST"}, {title: "KIND"}, {title: "ENTRIES", right: true}, {title: "AGE", right: true},
		{title: "DESCRIPTION", flex: true, min: 16}})
	v.tbl.empty = "no lists configured (scan.list_names / scan.custom_lists)"
	return v
}

func (v *listsView) name() string  { return TabLists }
func (v *listsView) title() string { return "Lists" }
func (v *listsView) about() string {
	return "The known-proxy, Tor, VPN, datacenter and abuse IP lists scans check addresses against " +
		"(`px lists`). Copies live in `<home>/lists` and are refreshed when older than `scan.lists_ttl`; `u` " +
		"downloads the stale ones now, `U` every one, in the background."
}

func (v *listsView) rebuild(m *Model) {
	s := m.styles
	rows := make([][]cell, len(v.rows))
	for i, r := range v.rows {
		age := cell{"never", s.Warn}
		if !r.meta.FetchedAt.IsZero() {
			age = cell{ago(r.meta.FetchedAt), s.Muted}
			if r.fresh {
				age.style = s.Good
			}
		}
		desc := cell{r.desc, s.Muted}
		if r.meta.Error != "" {
			desc = cell{r.meta.Error, s.Bad}
		}
		rows[i] = []cell{{r.meta.Name, s.Accent2}, plainCell(r.meta.Kind), plainCell(commas(r.meta.Entries)), age, desc}
	}
	v.tbl.setRows(rows)
}

func (v *listsView) resize(m *Model, w, h int) {
	v.w, v.h = w, h
	v.tbl.setHeight(h - 3)
	v.rebuild(m)
}

func (v *listsView) refresh(m *Model) tea.Cmd {
	v.seq++
	seq := v.seq
	mgr, err := m.listManager()
	return func() tea.Msg {
		if err != nil {
			return listsMsg{seq: seq, err: err}
		}
		desc := map[string]string{}
		for _, l := range mgr.Lists {
			desc[l.Name] = l.Desc
		}
		var rows []listRow
		for _, st := range mgr.Status() {
			rows = append(rows, listRow{meta: st, desc: desc[st.Name], fresh: mgr.Fresh(reputation.List{Name: st.Name})})
		}
		return listsMsg{seq: seq, rows: rows, dir: mgr.Dir, ttl: mgr.TTL}
	}
}

func (v *listsView) update(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case listsMsg:
		if msg.seq == v.seq {
			v.loaded, v.err = true, msg.err
			if msg.err == nil {
				v.rows, v.dir, v.ttl = msg.rows, msg.dir, msg.ttl
				v.rebuild(m)
			}
		}
	case listsDoneMsg:
		if m.activeView() == view(v) {
			return v.refresh(m)
		}
	}
	return nil
}

func (v *listsView) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, keys.Update):
		return m.updateLists(false)
	case key.Matches(msg, keys.ForceUpdate):
		return m.updateLists(true)
	}
	navigate(&v.tbl, msg, keys.Up)
	return nil
}

func (v *listsView) scroll(m *Model, d int) tea.Cmd {
	v.tbl.move(d)
	return nil
}

func (v *listsView) click(m *Model, _, y int) tea.Cmd {
	if i := v.tbl.rowAt(y); i >= 0 {
		v.tbl.cursor = i
		v.tbl.clamp()
	}
	return nil
}

func (v *listsView) render(m *Model) string {
	s := m.styles
	if v.err != nil {
		return s.Error.Render("  " + v.err.Error())
	}
	if !v.loaded {
		return s.Muted.Render("  reading the lists…")
	}
	body := fitLines(v.tbl.view(s, v.w), v.w, max(1, v.h-3))
	fresh, never := 0, 0
	for _, r := range v.rows {
		switch {
		case r.meta.FetchedAt.IsZero():
			never++
		case r.fresh:
			fresh++
		}
	}
	info := fmt.Sprintf("%s · %d fresh, %d never downloaded · refreshed when older than %s (scan.lists_ttl) · %d DNS blacklists (scan.dnsbl_zones)",
		paths.Pretty(v.dir), fresh, never, v.ttl, len(m.cfg.Scan.DNSBLZones))
	return body + "\n" + s.Faint.Render(" "+strings.Repeat("─", max(1, v.w-2))) + "\n " + s.Muted.Render(truncate(info, max(10, v.w-2))) +
		"\n " + s.Faint.Render("u: download stale lists · U: download all · px lists check IP looks one up")
}

func (v *listsView) status(m *Model) []string {
	if !v.loaded {
		return []string{"loading…"}
	}
	return []string{fmt.Sprintf("%d lists", len(v.rows)), "row " + v.tbl.position()}
}

func (v *listsView) help() []key.Binding { return []key.Binding{keys.Update, keys.ForceUpdate} }

func (v *listsView) hints() []hint {
	return []hint{{"↑↓", "move"}, {"u", "update stale"}, {"U", "update all"}}
}
