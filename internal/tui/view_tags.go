package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

// tagDesc explains the well-known tags (px tags --help).
var tagDesc = map[string]string{
	score.TagStable:      "heuristic: the last tags.stable_n checks / uses all succeeded",
	score.TagFast:        "heuristic: the last tags.fast_n timed successes were all under tags.fast_ms",
	score.TagFav:         "heuristic: used >= tags.fav_min_uses times for the command with >= tags.fav_min_rate ok",
	score.TagRecommended: "heuristic: the tags.recommend_top best ranked proxies (overall, or for the command)",
	score.TagClean:       "heuristic: full-scanned, on no list, anonymous or elite",
	score.TagElite:       "heuristic: the target sees neither your IP nor proxy headers",
	score.TagHTTPS:       "heuristic: TLS through the proxy works",
	score.TagFlaky:       "heuristic: alive, but most checks fail",
	score.TagTor:         "heuristic: a Tor exit",
	score.TagDatacenter:  "heuristic: a hosting / datacenter range",
	score.TagKnown:       "heuristic: on a known-proxy list (many sites block these)",
	"blocked":            "manual: route never picks it (route.exclude_tags); b toggles it in the Proxies tab",
	"pinned":             "manual: your label, usable with px --tag pinned <cmd>",
}

type tagsMsg struct {
	seq    int
	counts []db.TagCount
	err    error
}

type tagsView struct {
	baseView
	tbl    table
	all    []db.TagCount
	rows   []db.TagCount // filtered
	seq    int
	loaded bool
	err    error

	query      string
	search     textinput.Model
	searching  bool
	prevSearch string
}

func newTagsView(m *Model) *tagsView {
	v := &tagsView{}
	v.search = newInput(m, "/", "tag name")
	v.tbl.setColumns([]column{{title: "TAG", max: 40}, {title: "PROXIES", right: true}, {title: "KIND"}, {title: "SHARE", flex: true}})
	v.tbl.empty = "no tags yet: scans and uses add them (R refreshes now)"
	return v
}

func (v *tagsView) name() string  { return TabTags }
func (v *tagsView) title() string { return "Tags" }
func (v *tagsView) about() string {
	return "Every tag and how many proxies carry it. Heuristic tags (stable, fast, fav:<cmd>, recommended, clean, " +
		"elite, https, flaky, tor, datacenter, known) are recomputed after scans and uses, `R` recomputes them now; " +
		"every other tag is manual and never touched by a refresh. Enter lists the proxies carrying the tag."
}

func (v *tagsView) capturing() bool { return v.searching }

func labelOf(t db.TagCount) string {
	if t.Scope == "" {
		return t.Tag
	}
	return t.Tag + ":" + t.Scope
}

func (v *tagsView) rebuild(m *Model) {
	s := m.styles
	v.rows = v.rows[:0]
	most := 1
	for _, t := range v.all {
		if v.query == "" || strings.Contains(strings.ToLower(labelOf(t)), strings.ToLower(v.query)) {
			v.rows = append(v.rows, t)
			most = max(most, t.Count)
		}
	}
	rows := make([][]cell, len(v.rows))
	for i, t := range v.rows {
		kind := cell{"manual", s.Warn}
		if score.IsHeuristic(t.Tag) {
			kind = cell{"heuristic", s.Muted}
		}
		rows[i] = []cell{{labelOf(t), s.Tag}, plainCell(commas(t.Count)), kind,
			{bar(float64(t.Count)/float64(most), 24), s.Accent}}
	}
	v.tbl.setRows(rows)
}

func (v *tagsView) resize(m *Model, w, h int) {
	v.w, v.h = w, h
	styleInput(m, &v.search)
	v.search.Width = max(10, w-4)
	v.tbl.setHeight(h - 3)
	v.rebuild(m)
}

func (v *tagsView) refresh(m *Model) tea.Cmd {
	v.seq++
	seq := v.seq
	d, ctx := m.d, m.ctx
	return func() tea.Msg {
		c, err := d.TagCounts(ctx)
		return tagsMsg{seq: seq, counts: c, err: err}
	}
}

func (v *tagsView) update(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tagsMsg:
		if msg.seq != v.seq {
			return nil
		}
		v.loaded, v.err = true, msg.err
		if msg.err == nil {
			v.all = msg.counts
			m.tagLabels = nil
			for _, t := range v.all {
				m.tagLabels = append(m.tagLabels, labelOf(t))
			}
			v.rebuild(m)
		}
	case retagDoneMsg, rankDoneMsg, tagsChangedMsg:
		if m.activeView() == view(v) {
			return v.refresh(m)
		}
	default:
		if v.searching {
			var cmd tea.Cmd
			v.search, cmd = v.search.Update(msg)
			return cmd
		}
	}
	return nil
}

func (v *tagsView) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	if v.searching {
		switch msg.String() {
		case "enter":
			v.searching = false
			v.search.Blur()
			return nil
		case "esc":
			v.searching = false
			v.search.Blur()
			v.query = v.prevSearch
			v.rebuild(m)
			return nil
		}
		var cmd tea.Cmd
		v.search, cmd = v.search.Update(msg)
		v.query = v.search.Value()
		v.tbl.reset()
		v.rebuild(m)
		return cmd
	}
	if navigate(&v.tbl, msg, keys.Up) {
		return nil
	}
	switch {
	case key.Matches(msg, keys.Enter):
		if i := v.tbl.selected(); i >= 0 {
			t := v.rows[i]
			return m.showTag(t.Tag, t.Scope)
		}
	case key.Matches(msg, keys.Retag):
		return m.retag()
	case key.Matches(msg, keys.Search):
		v.searching = true
		v.prevSearch = v.query
		v.search.SetValue(v.query)
		v.search.CursorEnd()
		return v.search.Focus()
	case key.Matches(msg, keys.Clear):
		v.query = ""
		v.rebuild(m)
	}
	return nil
}

func (v *tagsView) click(m *Model, _, y int) tea.Cmd {
	i := v.tbl.rowAt(y)
	if i < 0 || v.searching {
		return nil
	}
	if i == v.tbl.cursor {
		t := v.rows[i]
		return m.showTag(t.Tag, t.Scope)
	}
	v.tbl.cursor = i
	v.tbl.clamp()
	return nil
}

func (v *tagsView) scroll(m *Model, d int) tea.Cmd {
	v.tbl.move(d)
	return nil
}

func (v *tagsView) render(m *Model) string {
	s := m.styles
	if v.err != nil {
		return s.Error.Render("  " + v.err.Error())
	}
	if !v.loaded {
		return s.Muted.Render("  loading tags…")
	}
	body := fitLines(v.tbl.view(s, v.w), v.w, max(1, v.h-3))
	desc := ""
	if i := v.tbl.selected(); i >= 0 {
		t := v.rows[i]
		d, ok := tagDesc[t.Tag]
		if !ok {
			d = "manual: your own label (never touched by a refresh)"
		}
		if t.Scope != "" {
			d += fmt.Sprintf(" · scope %q", t.Scope)
		}
		desc = s.Tag.Render(labelOf(t)) + "  " + s.Muted.Render(d)
	}
	return body + "\n" + s.Faint.Render(" "+strings.Repeat("─", max(1, v.w-2))) + "\n " + desc + "\n " +
		s.Faint.Render("enter: the proxies carrying it · R: recompute every heuristic tag now (tags.* thresholds)")
}

func (v *tagsView) status(m *Model) []string {
	if !v.loaded {
		return []string{"loading…"}
	}
	segs := []string{fmt.Sprintf("%d tags", len(v.rows))}
	if v.query != "" {
		segs = append(segs, "/"+v.query)
	}
	return segs
}

func (v *tagsView) footer(m *Model) string {
	if v.searching {
		return v.search.View()
	}
	return ""
}

func (v *tagsView) help() []key.Binding {
	return []key.Binding{bind("enter", "show the proxies carrying the tag (Proxies tab)", "enter"), keys.Retag,
		bind("/", "filter tag names", "/"), bind("X", "clear the filter", "X")}
}

func (v *tagsView) hints() []hint {
	return []hint{{"↑↓", "move"}, {"enter", "proxies"}, {"R", "refresh tags"}, {"/", "filter"}}
}
