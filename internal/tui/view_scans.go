package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/scan"
)

type scansMsg struct {
	seq  int
	runs []*db.ScanRun
	err  error
}

type scanCountMsg struct {
	seq    int
	n      int
	paused *db.ScanRun // the newest unfinished run of the mode, if any
	err    error
}

var scanModeHelp = map[string]string{
	scan.ModeQuick:      "one probe per proxy: alive or dead, latency",
	scan.ModeFull:       "every check, gently: probes, exit IP, HTTPS / TLS interception, anonymity, protocols, lists, DNSBL, geo",
	scan.ModeReputation: "IP lists + DNS blacklists only (no traffic through the proxies)",
	scan.ModeGeo:        "geolocation / ASN of exit IPs only",
	scan.ModeProtocols:  "which of http / socks5 / socks4 each address speaks",
}

var scanSelHelp = map[string]string{
	"all":      "every proxy in the database",
	"alive":    "the proxies alive at their last check",
	"filter":   "what the Proxies tab shows (its filter and search)",
	"selected": "the proxy selected in the Proxies tab",
}

// scanForm is the "new scan" panel of the Scans tab.
type scanForm struct {
	field   int // 0 mode, 1 selection, 2 restart
	mode    string
	sel     string
	restart bool
	count   int // -1 while counting
	seq     int
	label   string
	err     string
	paused  *db.ScanRun
}

// scansView lists the scan runs, starts scans and shows the running one.
type scansView struct {
	baseView
	tbl    table
	runs   []*db.ScanRun
	seq    int
	loaded bool
	err    error

	form *scanForm

	snap      scanSnap // the latest poll of the current / last scan
	hasSnap   bool
	dismissed bool
	lastRuns  time.Time // last reload of the runs while scanning
}

func newScansView() *scansView {
	v := &scansView{}
	v.tbl.setColumns([]column{{title: "ID", right: true}, {title: "MODE"}, {title: "STATE"}, {title: "PROGRESS"},
		{title: "DONE", right: true}, {title: "%", right: true}, {title: "STARTED"}, {title: "TOOK", right: true}})
	v.tbl.empty = "no scans yet: n starts one (quick = alive / dead, full = every check)"
	return v
}

func (v *scansView) name() string  { return TabScans }
func (v *scansView) title() string { return "Scans" }
func (v *scansView) about() string {
	return "Scan runs, newest first, and the scan running in this TUI. `n` starts one: pick the mode (quick, full, " +
		"reputation, geo, protocols), the selection (every proxy, the alive ones, the Proxies tab's filter, or the " +
		"selected proxy) and whether to restart instead of resuming. Progress, rate, ETA and the alive proxies " +
		"stream in while it runs; `x` pauses it (everything measured is kept, the same scan resumes later). One scan " +
		"runs at a time; quitting pauses it. `k` recomputes scores, ranks and tags like `px rank`."
}

func (v *scansView) capturing() bool { return v.form != nil }

func (v *scansView) rebuild(m *Model) {
	s := m.styles
	rows := make([][]cell, len(v.runs))
	for i, r := range v.runs {
		done, total, state := r.Done, r.Total, r.State
		if v.hasSnap && !v.snap.finished && v.snap.started && r.ID == v.snap.run {
			done, total, state = v.snap.done, max(v.snap.total, r.Total), db.ScanRunning
		}
		frac := float64(done) / float64(max(total, 1))
		took := "-"
		switch {
		case !r.FinishedAt.IsZero():
			took = durText(r.FinishedAt.Sub(r.StartedAt).Milliseconds())
		case state == db.ScanRunning:
			took = durText(time.Since(r.StartedAt).Milliseconds()) + "…"
		}
		rows[i] = []cell{{fmt.Sprint(r.ID), s.Muted}, plainCell(r.Mode), {state, s.scanState(state)},
			{bar(frac, 16), s.scanState(state)}, plainCell(commas(done) + "/" + commas(total)),
			plainCell(fmt.Sprintf("%.1f", 100*frac)), {r.StartedAt.Local().Format("2006-01-02 15:04"), s.Muted}, plainCell(took)}
	}
	v.tbl.setRows(rows)
}

func (v *scansView) resize(m *Model, w, h int) {
	v.w, v.h = w, h
	v.tbl.setHeight(v.tableHeight(m))
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

// progressed takes a poll of the running scan; the runs are reloaded now
// and then while the Scans tab is visible.
func (v *scansView) progressed(m *Model, sn scanSnap) tea.Cmd {
	v.snap, v.hasSnap = sn, true
	v.tbl.setHeight(v.tableHeight(m))
	v.rebuild(m)
	if m.activeView() != view(v) {
		return nil
	}
	known := false
	for _, r := range v.runs {
		if r.ID == sn.run {
			known = true
		}
	}
	if sn.finished || (sn.started && !known) || time.Since(v.lastRuns) > 1500*time.Millisecond {
		v.lastRuns = time.Now()
		return v.refresh(m)
	}
	return nil
}

// reset forgets the previous scan's panel (a new scan starts).
func (v *scansView) reset() {
	v.snap, v.hasSnap, v.dismissed, v.lastRuns = scanSnap{}, false, false, time.Time{}
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
	case scanCountMsg:
		if f := v.form; f != nil && msg.seq == f.seq {
			f.count, f.paused = msg.n, msg.paused
			if msg.err != nil {
				f.err = msg.err.Error()
			}
		}
	case rankDoneMsg, scanEndedMsg:
		if m.activeView() == view(v) {
			return v.refresh(m)
		}
	}
	return nil
}

// ---- the new-scan form -------------------------------------------------------------

func (v *scansView) openForm(m *Model, mode string) tea.Cmd {
	if m.opts.StartScan == nil {
		return m.notify(levelErr, "scans cannot be started from this TUI (no scanner)")
	}
	if m.scanRunning() {
		return m.notify(levelWarn, "a %s scan is running: x pauses it first", m.scan.req.Mode)
	}
	v.form = &scanForm{mode: mode, sel: "all"}
	v.tbl.setHeight(v.tableHeight(m))
	return v.count(m)
}

// count asks how many proxies the form's selection covers.
func (v *scansView) count(m *Model) tea.Cmd {
	f := v.form
	f.seq++
	f.count, f.err, f.label, f.paused = -1, "", "", nil
	req, err := m.scanRequest(f.mode, f.sel, f.restart)
	if err != nil {
		f.count, f.err = 0, err.Error()
		return nil
	}
	f.label = req.Label
	seq, d, ctx, mode := f.seq, m.d, m.ctx, f.mode
	return func() tea.Msg {
		n, err := d.CountProxies(ctx, req.Filter)
		out := scanCountMsg{seq: seq, n: n, err: err}
		if p, err := d.ResumableScanRun(ctx, mode); err == nil {
			out.paused = p
		}
		return out
	}
}

func (v *scansView) formKey(m *Model, msg tea.KeyMsg) tea.Cmd {
	f := v.form
	step := 0
	switch k := msg.String(); k {
	case "esc":
		v.form = nil
		v.tbl.setHeight(v.tableHeight(m))
		return nil
	case "enter":
		if f.err != "" {
			return m.notify(levelWarn, "%s", f.err)
		}
		if f.count == 0 {
			return m.notify(levelWarn, "nothing to scan: the selection matches no proxy")
		}
		req, err := m.scanRequest(f.mode, f.sel, f.restart)
		if err != nil {
			return m.notify(levelErr, "%v", err)
		}
		v.form = nil
		v.tbl.setHeight(v.tableHeight(m))
		return m.startScan(req)
	case "up", "shift+tab", "k":
		f.field = (f.field + 2) % 3
		return nil
	case "down", "tab", "j":
		f.field = (f.field + 1) % 3
		return nil
	case "left", "h":
		step = -1
	case "right", "l", " ":
		step = 1
	case "1", "2", "3", "4", "5":
		f.mode = scan.Modes[int(k[0]-'1')]
	case "a":
		f.sel = "all"
	case "A":
		f.sel = "alive"
	case "f":
		f.sel = "filter"
	case "s":
		f.sel = "selected"
	case "x", "r":
		f.restart = !f.restart
		return nil
	default:
		return nil
	}
	if step != 0 {
		switch f.field {
		case 0:
			f.mode = cycle(scan.Modes, f.mode, step)
		case 1:
			f.sel = cycle(scanSelections, f.sel, step)
		case 2:
			f.restart = !f.restart
			return nil
		}
	}
	return v.count(m)
}

func (v *scansView) formView(m *Model) string {
	s := m.styles
	f := v.form
	field := func(i int, label, value, help string) string {
		mark := "  "
		if i == f.field {
			mark = s.Marker.Render("▸ ")
		}
		val := s.Accent2.Render(pad("‹ "+value+" ›", 15, false))
		return " " + mark + s.Accent.Render(pad(label, 10, false)) + " " + val + " " + s.Muted.Render(help)
	}
	target := s.Muted.Render("counting…")
	switch {
	case f.err != "":
		target = s.Error.Render(f.err)
	case f.count >= 0:
		target = s.Bold.Render(plural(f.count, "proxy", "proxies")) + s.Muted.Render(" · "+f.label)
	}
	restart := "no"
	restartHelp := "resume a paused run of the same mode and selection"
	if f.restart {
		restart, restartHelp = "yes", "start over (a paused run is set aside)"
	}
	lines := []string{
		" " + s.Title.Render("New scan"),
		field(0, "mode", f.mode, scanModeHelp[f.mode]),
		field(1, "selection", f.sel, scanSelHelp[f.sel]),
		field(2, "restart", restart, restartHelp),
		"   " + s.Muted.Render(pad("targets", 10, false)) + " " + target,
	}
	if p := f.paused; p != nil && !f.restart {
		lines = append(lines, "   "+s.Warn.Render(fmt.Sprintf("paused %s scan #%d (%s/%s) resumes if its selection is the same",
			p.Mode, p.ID, commas(p.Done), commas(p.Total))))
	}
	return strings.Join(lines, "\n")
}

// ---- the running scan's panel ------------------------------------------------------

func (v *scansView) showPanel() bool { return v.hasSnap && !v.dismissed }

func (v *scansView) panelView(m *Model) string {
	s := m.styles
	sn := v.snap
	req := ScanRequest{}
	if m.scan != nil {
		req = m.scan.req
	}
	state, st := "running", s.Info
	switch {
	case sn.finished && errors.Is(sn.err, context.Canceled):
		state, st = "stopped", s.Warn
	case sn.finished && sn.err != nil:
		state, st = "failed", s.Bad
	case sn.finished && sn.sum.Interrupted:
		state, st = "paused", s.Warn
	case sn.finished:
		state, st = "done", s.Good
	case sn.stopping:
		state, st = "pausing…", s.Warn
	case !sn.started:
		state, st = "starting…", s.Muted
	}
	elapsed := time.Since(sn.began)
	if sn.finished {
		elapsed = sn.sum.Elapsed
	}
	run := ""
	if sn.run > 0 {
		run = fmt.Sprintf(" #%d", sn.run)
	}
	head := " " + s.Title.Render(strings.ToUpper(req.Mode)+" scan"+run) + s.Muted.Render(" · "+req.Label+" · ") + st.Render(state) +
		s.Muted.Render(" · "+durText(elapsed.Milliseconds()))
	frac := float64(sn.done) / float64(max(sn.total, 1))
	rate := float64(sn.done-sn.base) / max(elapsed.Seconds(), 0.001)
	eta := "-"
	if rate > 0 && !sn.finished && sn.total > sn.done {
		eta = durText(int64(float64(sn.total-sn.done) / rate * 1000))
	}
	bw := max(10, min(40, v.w-60))
	prog := "   " + st.Render(bar(frac, bw)) + fmt.Sprintf("  %s/%s  %5.1f%%  %s/s  ETA %s", commas(sn.done), commas(sn.total),
		100*frac, fmt.Sprintf("%.1f", rate), eta)
	var counts []string
	if liveness(req.Mode) {
		counts = append(counts, s.Good.Render(fmt.Sprintf("%d alive", sn.alive)), s.Bad.Render(fmt.Sprintf("%d dead", sn.dead)))
	}
	counts = append(counts, s.Warn.Render(fmt.Sprintf("%d listed", sn.listed)))
	if sn.mitm > 0 {
		counts = append(counts, s.Bad.Render(fmt.Sprintf("%d intercept TLS", sn.mitm)))
	}
	line3 := "   " + strings.Join(counts, s.Muted.Render(" · "))
	if sn.last != "" {
		line3 += s.Muted.Render("   last ") + s.Accent2.Render(sn.last)
	}
	lines := []string{head, prog, line3}
	if sn.extras != "" && !sn.finished {
		lines = append(lines, "   "+s.Faint.Render(strings.TrimSpace(strings.Trim(sn.extras, " ()"))))
	}
	for _, l := range sn.log {
		lines = append(lines, "   "+s.Muted.Render(l))
	}
	if sn.finished && sn.err != nil && !errors.Is(sn.err, context.Canceled) {
		lines = append(lines, "   "+s.Error.Render(sn.err.Error()))
	}
	// The alive ones so far, newest first.
	if n := len(sn.found); n > 0 && v.h >= 22 {
		show := min(n, 6)
		lines = append(lines, " "+s.Accent.Render(fmt.Sprintf("alive so far (%d)", sn.alive)))
		ft := table{noCursor: true}
		ft.setColumns([]column{{title: "PROXY", max: 46}, {title: "MS", right: true}, {title: "CC"}, {title: "ANON"},
			{title: "BL", right: true}, {title: "TLS"}})
		var rows [][]cell
		for i := n - 1; i >= n-show; i-- {
			r := sn.found[i]
			bl := cell{fmt.Sprint(r.Listed), s.Muted}
			if r.Listed > 0 {
				bl.style = s.Bad
			}
			rows = append(rows, []cell{{r.URL, s.Accent2}, {msText(r.MS), s.ms(r.MS)}, {dash(r.Country), s.Info},
				{anonShort(r.Anonymity), s.anon(r.Anonymity)}, bl, tlsCell(s, r.HTTPS, r.MITM)})
		}
		ft.setRows(rows)
		ft.setHeight(len(rows) + 1)
		lines = append(lines, ft.view(s, v.w))
	}
	return strings.Join(lines, "\n")
}

// top is what sits above the runs table.
func (v *scansView) top(m *Model) string {
	switch {
	case v.form != nil:
		return v.formView(m)
	case v.showPanel():
		return v.panelView(m)
	}
	return ""
}

func (v *scansView) tableHeight(m *Model) int {
	top := 0
	if t := v.top(m); t != "" {
		top = strings.Count(t, "\n") + 2 // + the separator
	}
	return max(3, v.h-3-top)
}

func (v *scansView) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	if v.form != nil {
		return v.formKey(m, msg)
	}
	switch {
	case key.Matches(msg, keys.Rank): // k ranks here, so up is only the arrow
		return m.rank("")
	case key.Matches(msg, keys.NewScan):
		return v.openForm(m, scan.ModeQuick)
	case key.Matches(msg, keys.StopScan):
		return m.askStopScan()
	case key.Matches(msg, keys.Dismiss):
		if v.hasSnap && v.snap.finished {
			v.dismissed = true
			v.tbl.setHeight(v.tableHeight(m))
		}
		return nil
	}
	navigate(&v.tbl, msg, keys.UpNoK)
	return nil
}

func (v *scansView) scroll(m *Model, d int) tea.Cmd {
	v.tbl.move(d)
	return nil
}

func (v *scansView) click(m *Model, _, y int) tea.Cmd {
	off := 0
	if t := v.top(m); t != "" {
		off = strings.Count(t, "\n") + 2
	}
	if i := v.tbl.rowAt(y - off); i >= 0 {
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
	rule := s.Faint.Render(" " + strings.Repeat("─", max(1, v.w-2)))
	var b strings.Builder
	if t := v.top(m); t != "" {
		b.WriteString(t + "\n" + rule + "\n")
	}
	th := v.tableHeight(m)
	if !v.loaded {
		b.WriteString(fitLines(s.Muted.Render("  loading scan runs…"), v.w, th))
	} else {
		b.WriteString(fitLines(v.tbl.view(s, v.w), v.w, th))
	}
	running := 0
	for _, r := range v.runs {
		if r.State == db.ScanRunning {
			running++
		}
	}
	info := fmt.Sprintf("rank order: %s (scan.rank) · %d running · workers %d (quick %d) · timeout %s",
		m.cfg.Scan.Rank, running, m.cfg.Scan.Workers, m.cfg.Scan.QuickWorkers, m.cfg.Scan.Timeout)
	keysLine := "n: new scan · x: pause it · k: recompute scores, ranks and tags · F in Proxies: full-scan one proxy"
	b.WriteString("\n" + rule + "\n " + s.Muted.Render(truncate(info, max(10, v.w-2))) + "\n " + s.Faint.Render(truncate(keysLine, max(10, v.w-2))))
	return b.String()
}

func (v *scansView) status(m *Model) []string {
	if !v.loaded {
		return []string{"loading…"}
	}
	// A running scan shows as a job at the front of the status bar.
	return []string{fmt.Sprintf("%d runs", len(v.runs)), "row " + v.tbl.position()}
}

func (v *scansView) footer(m *Model) string {
	if v.form != nil {
		return m.hintLine([]hint{{"enter", "start"}, {"↑↓", "field"}, {"←→", "change"}, {"1-5", "mode"},
			{"a/A/f/s", "all/alive/filter/selected"}, {"x", "restart"}, {"esc", "cancel"}})
	}
	return ""
}

func (v *scansView) help() []key.Binding {
	return []key.Binding{keys.NewScan, keys.StopScan, keys.Dismiss, keys.Rank, bind("↑/↓", "move (k is rank here)", "up"),
		bind("form: ↑↓ / tab", "choose the field", "up"), bind("form: ←→ / space", "change its value", "left"),
		bind("form: 1-5", "mode: quick, full, reputation, geo, protocols", "1"),
		bind("form: a A f s", "selection: all, alive, the proxy filter, the selected proxy", "a"),
		bind("form: x", "restart instead of resuming", "x"), bind("form: enter / esc", "start / cancel", "enter")}
}

func (v *scansView) hints() []hint {
	if v.hasSnap && !v.snap.finished {
		return []hint{{"x", "pause the scan"}, {"↑↓", "move"}, {"k", "rank"}}
	}
	return []hint{{"n", "new scan"}, {"↑↓", "move"}, {"k", "rank"}, {"ctrl+r", "reload"}}
}
