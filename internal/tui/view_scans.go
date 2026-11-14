package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

type scansMsg struct {
	seq  int
	runs []*db.ScanRun
	err  error
}

// scansView lists the scan runs (px scan runs) with their progress.
type scansView struct {
	baseView
	tbl    table
	runs   []*db.ScanRun
	seq    int
	loaded bool
	err    error
}

func newScansView() *scansView {
	v := &scansView{}
	v.tbl.setColumns([]column{{title: "ID", right: true}, {title: "MODE"}, {title: "STATE"}, {title: "PROGRESS"},
		{title: "DONE", right: true}, {title: "%", right: true}, {title: "STARTED"}, {title: "TOOK", right: true}})
	v.tbl.empty = "no scans yet: px scan full (every check) or px scan quick (alive / dead)"
	return v
}

func (v *scansView) name() string  { return TabScans }
func (v *scansView) title() string { return "Scans" }
func (v *scansView) about() string {
	return "The scan runs recorded in the database, newest first, with their progress; a running scan (from " +
		"another terminal) advances on every refresh. Scans start from the shell (`px scan full`, `px scan " +
		"quick`, ...); `k` recomputes every proxy's speed / clean / stability score and the rank (`scan.rank`), " +
		"then refreshes the heuristic tags, like `px rank`."
}

func (v *scansView) rebuild(m *Model) {
	s := m.styles
	rows := make([][]cell, len(v.runs))
	for i, r := range v.runs {
		frac := float64(r.Done) / float64(max(r.Total, 1))
		took := "-"
		switch {
		case !r.FinishedAt.IsZero():
			took = durText(r.FinishedAt.Sub(r.StartedAt).Milliseconds())
		case r.State == db.ScanRunning:
			took = durText(time.Since(r.StartedAt).Milliseconds()) + "…"
		}
		rows[i] = []cell{{fmt.Sprint(r.ID), s.Muted}, plainCell(r.Mode), {r.State, s.scanState(r.State)},
			{bar(frac, 16), s.scanState(r.State)}, plainCell(commas(r.Done) + "/" + commas(r.Total)),
			plainCell(fmt.Sprintf("%.1f", 100*frac)), {r.StartedAt.Local().Format("2006-01-02 15:04"), s.Muted}, plainCell(took)}
	}
	v.tbl.setRows(rows)
}

func (v *scansView) resize(m *Model, w, h int) {
	v.w, v.h = w, h
	v.tbl.setHeight(h - 3)
	v.rebuild(m)
}

func (v *scansView) refresh(m *Model) tea.Cmd {
	v.seq++
	seq := v.seq
	d, ctx := m.d, m.ctx
	return func() tea.Msg {
		runs, err := d.ListScanRuns(ctx, 200)
		return scansMsg{seq: seq, runs: runs, err: err}
	}
}

func (v *scansView) update(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case scansMsg:
		if msg.seq == v.seq {
			v.loaded, v.err = true, msg.err
			if msg.err == nil {
				v.runs = msg.runs
				v.rebuild(m)
			}
		}
	case rankDoneMsg:
		if m.activeView() == view(v) {
			return v.refresh(m)
		}
	}
	return nil
}

func (v *scansView) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	// k ranks here, so up is only the arrow.
	if key.Matches(msg, keys.Rank) {
		return m.rank("")
	}
	navigate(&v.tbl, msg, keys.UpNoK)
	return nil
}

func (v *scansView) scroll(m *Model, d int) tea.Cmd {
	v.tbl.move(d)
	return nil
}

func (v *scansView) click(m *Model, _, y int) tea.Cmd {
	if i := v.tbl.rowAt(y); i >= 0 {
		v.tbl.cursor = i
		v.tbl.clamp()
	}
	return nil
}

func (v *scansView) render(m *Model) string {
	s := m.styles
	if v.err != nil {
		return s.Error.Render("  " + v.err.Error())
	}
	if !v.loaded {
		return s.Muted.Render("  loading scan runs…")
	}
	body := fitLines(v.tbl.view(s, v.w), v.w, max(1, v.h-3))
	running := 0
	for _, r := range v.runs {
		if r.State == db.ScanRunning {
			running++
		}
	}
	info := fmt.Sprintf("rank order: %s (scan.rank) · %d running", m.cfg.Scan.Rank, running)
	return body + "\n" + s.Faint.Render(" "+strings.Repeat("─", max(1, v.w-2))) + "\n " + s.Muted.Render(info) + "\n " +
		s.Faint.Render("k: recompute scores, ranks and tags · scans start from the shell: px scan full | quick | reputation | geo")
}

func (v *scansView) status(m *Model) []string {
	if !v.loaded {
		return []string{"loading…"}
	}
	return []string{fmt.Sprintf("%d runs", len(v.runs)), "row " + v.tbl.position()}
}

func (v *scansView) help() []key.Binding {
	return []key.Binding{keys.Rank, bind("↑/↓", "move (k is rank here)", "up")}
}

func (v *scansView) hints() []hint {
	return []hint{{"↑↓", "move"}, {"k", "rank"}, {"ctrl+r", "reload"}}
}
