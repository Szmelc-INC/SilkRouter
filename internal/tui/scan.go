package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/scan"
)

// ---- the contract with the CLI -----------------------------------------------------

// ScanRequest is a scan the TUI asks Options.StartScan to run.
type ScanRequest struct {
	Mode      string    // one of scan.Modes
	Filter    db.Filter // the selection (sort / paging fields are ignored)
	Selection string    // "all", "alive", "filter" or "selected"
	Label     string    // the selection in words ("alive proxies", "proxy #42", ...)
	Restart   bool      // start over instead of resuming a paused run of the same selection
}

// ScanProgress is one update from a running scan: Started once the run row
// exists (Run, Total, Done = already done when resumed, Extras), Report once
// per scanned proxy (with Done / Total), Message for log lines.
type ScanProgress struct {
	Run     int64
	Started bool
	Extras  string
	Total   int
	Done    int
	Report  *scan.Report
	Message string
}

// ScanSummary is how a scan ended; Interrupted = paused (resumable).
type ScanSummary struct {
	Run                 int64
	Mode, State         string
	Total, Done         int
	Alive, Dead, Listed int64
	Elapsed             time.Duration
	Ranked              int
	Tags                map[string]int
	Resumed             bool
	Interrupted         bool
}

// ScanFunc runs one scan and returns when it is done or ctx is cancelled
// (which pauses it, keeping everything measured). progress may be called
// from several goroutines.
type ScanFunc func(ctx context.Context, cfg *config.Config, req ScanRequest, progress func(ScanProgress)) (ScanSummary, error)

// ErrNoMatch is what a ScanFunc returns when the selection matches no proxy.
var ErrNoMatch = errors.New("no proxies match")

// Selections a scan can cover.
var scanSelections = []string{"all", "alive", "filter", "selected"}

// ---- the running scan -----------------------------------------------------------------

const (
	scanPollEvery = 200 * time.Millisecond
	scanKeepFound = 200 // alive proxies kept for the live list
)

type scanPollMsg struct{}

// scanEndedMsg tells the views a scan finished (their data changed).
type scanEndedMsg struct{}

// scanSnap is what the UI shows of a scan; the scan goroutine writes it,
// the UI copies it on every poll.
type scanSnap struct {
	run                       int64
	started                   bool
	extras                    string
	total, done, base         int
	alive, dead, listed, mitm int
	last                      string
	found                     []scan.Report // alive ones, newest last
	log                       []string      // the latest messages
	began                     time.Time
	finished                  bool
	stopping                  bool
	sum                       ScanSummary
	err                       error
}

// liveness reports whether the mode tests proxies (alive / dead counts).
func liveness(mode string) bool {
	return mode == scan.ModeQuick || mode == scan.ModeFull || mode == scan.ModeProtocols
}

type scanSession struct {
	job    int
	req    ScanRequest
	cancel context.CancelFunc
	done   chan struct{} // closed once the ScanFunc returned

	mu       sync.Mutex
	snap     scanSnap
	reported bool // the end was shown (UI goroutine only)
}

func (s *scanSession) progress(p ScanProgress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn := &s.snap
	if p.Message != "" {
		sn.log = append(sn.log, p.Message)
		if len(sn.log) > 3 {
			sn.log = sn.log[len(sn.log)-3:]
		}
	}
	if p.Started {
		sn.started, sn.run, sn.extras = true, p.Run, p.Extras
		sn.total, sn.done, sn.base = p.Total, p.Done, p.Done
		sn.began = time.Now()
	}
	if r := p.Report; r != nil {
		sn.done, sn.total = p.Done, max(sn.total, p.Total)
		sn.last = r.URL
		switch {
		case r.Alive:
			sn.alive++
			sn.found = append(sn.found, *r)
			if len(sn.found) > scanKeepFound {
				sn.found = sn.found[len(sn.found)-scanKeepFound:]
			}
		case liveness(s.req.Mode):
			sn.dead++
		}
		if r.Listed > 0 {
			sn.listed++
		}
		if r.MITM != nil && *r.MITM {
			sn.mitm++
		}
	}
}

func (s *scanSession) snapshot() scanSnap {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn := s.snap
	sn.found = append([]scan.Report(nil), s.snap.found...)
	sn.log = append([]string(nil), s.snap.log...)
	return sn
}

func (s *scanSession) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// run executes the ScanFunc (in its own goroutine).
func (s *scanSession) run(ctx context.Context, f ScanFunc, cfg *config.Config) {
	defer close(s.done)
	var sum ScanSummary
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("scan crashed: %v", r)
			}
		}()
		sum, err = f(ctx, cfg, s.req, s.progress)
	}()
	s.mu.Lock()
	s.snap.finished, s.snap.sum, s.snap.err = true, sum, err
	s.mu.Unlock()
}

// wait blocks until the scan returned or d passed.
func (s *scanSession) wait(d time.Duration) bool {
	select {
	case <-s.done:
		return true
	case <-time.After(d):
		return false
	}
}

// ---- model glue ----------------------------------------------------------------------

func (m *Model) scanRunning() bool { return m.scan != nil && m.scan.running() }

// scanRequest builds the request for a selection ("all", "alive", "filter",
// "selected").
func (m *Model) scanRequest(mode, sel string, restart bool) (ScanRequest, error) {
	if !contains(scan.Modes, mode) {
		return ScanRequest{}, fmt.Errorf("unknown scan mode %q (want one of: %s)", mode, strings.Join(scan.Modes, ", "))
	}
	req := ScanRequest{Mode: mode, Selection: sel, Restart: restart}
	switch sel {
	case "all":
		req.Label = "every proxy"
	case "alive":
		req.Filter, req.Label = db.Filter{Status: []string{db.StatusAlive}}, "alive proxies"
	case "filter":
		req.Filter = m.prox.f.dbFilter()
		req.Filter.Limit, req.Filter.Offset, req.Filter.WithTags = 0, 0, false
		req.Label = "the proxy filter (" + strings.Join(m.prox.f.selectionSegments(), " · ") + ")"
	case "selected":
		p := m.prox.current()
		if p == nil {
			return req, errors.New("no proxy selected in the Proxies tab")
		}
		req.Filter, req.Label = db.Filter{IDs: []int64{p.ID}}, fmt.Sprintf("proxy #%d %s", p.ID, p.Key())
	default:
		return req, fmt.Errorf("unknown selection %q (want one of: %s)", sel, strings.Join(scanSelections, ", "))
	}
	return req, nil
}

// startScan starts req in the background; only one scan runs at a time.
func (m *Model) startScan(req ScanRequest) tea.Cmd {
	if m.opts.StartScan == nil {
		return m.notify(levelErr, "scans cannot be started from this TUI (no scanner); use %s scan %s", m.opts.Prog, req.Mode)
	}
	if m.scanRunning() {
		sn := m.scan.snapshot()
		return m.notify(levelWarn, "a %s scan is running (%d/%d): one at a time (x in the Scans tab pauses it)",
			m.scan.req.Mode, sn.done, sn.total)
	}
	job, spin := m.startJob("scan " + req.Mode)
	ctx, cancel := context.WithCancel(m.ctx)
	s := &scanSession{job: job, req: req, cancel: cancel, done: make(chan struct{})}
	s.snap.began = time.Now()
	m.scan = s
	m.scans.reset()
	shown := m.scans.progressed(m, s.snapshot()) // the "starting…" panel
	go s.run(ctx, m.opts.StartScan, m.cfg)
	restart := ""
	if req.Restart {
		restart = " (restart)"
	}
	return tea.Batch(spin, shown, m.notify(levelInfo, "%s scan of %s started%s", req.Mode, req.Label, restart), m.pollScan())
}

func (m *Model) pollScan() tea.Cmd {
	return m.tick(scanPollEvery, func(time.Time) tea.Msg { return scanPollMsg{} })
}

// onScanPoll copies the scan's progress into the UI and reports its end.
func (m *Model) onScanPoll() tea.Cmd {
	s := m.scan
	if s == nil || s.reported {
		return nil
	}
	sn := s.snapshot()
	cmds := []tea.Cmd{m.scans.progressed(m, sn)}
	if !sn.finished {
		if _, ok := m.jobs[s.job]; ok {
			m.jobs[s.job] = fmt.Sprintf("scan %s %s/%s", s.req.Mode, commas(sn.done), commas(sn.total))
		}
		return tea.Batch(append(cmds, m.pollScan())...)
	}
	s.reported = true
	delete(m.jobs, s.job)
	if m.ask != nil && m.ask.scan {
		m.ask = nil // nothing left to pause
	}
	cmds = append(cmds, m.scanEndToast(sn), func() tea.Msg { return scanEndedMsg{} })
	return tea.Batch(cmds...)
}

func (m *Model) scanEndToast(sn scanSnap) tea.Cmd {
	mode := m.scan.req.Mode
	switch {
	case errors.Is(sn.err, ErrNoMatch):
		return m.notify(levelWarn, "%s scan: nothing to scan (no proxy matches %s)", mode, m.scan.req.Label)
	case errors.Is(sn.err, context.Canceled):
		return m.notify(levelInfo, "%s scan stopped before it started", mode)
	case sn.err != nil:
		return m.notify(levelErr, "%s scan: %v", mode, sn.err)
	}
	sum := sn.sum
	counts := fmt.Sprintf("%s/%s in %s", commas(sum.Done), commas(sum.Total), durText(sum.Elapsed.Milliseconds()))
	if liveness(mode) {
		counts += fmt.Sprintf(" · %d alive · %d dead", sum.Alive, sum.Dead)
	}
	if sum.Listed > 0 {
		counts += fmt.Sprintf(" · %d listed", sum.Listed)
	}
	if sum.Ranked > 0 {
		counts += fmt.Sprintf(" · ranked %d", sum.Ranked)
	}
	if sum.Interrupted {
		return m.notify(levelWarn, "%s scan #%d paused at %s (the same scan resumes it)", mode, sum.Run, counts)
	}
	return m.notify(levelOK, "%s scan #%d done: %s", mode, sum.Run, counts)
}

// askStopScan asks before pausing the running scan.
func (m *Model) askStopScan() tea.Cmd {
	if !m.scanRunning() {
		return m.notify(levelInfo, "no scan is running")
	}
	sn := m.scan.snapshot()
	m.ask = &question{scan: true,
		text: fmt.Sprintf("pause the %s scan #%d at %s/%s? (resumable: start the same scan again) [y/N]",
			m.scan.req.Mode, sn.run, commas(sn.done), commas(sn.total)),
		yes: m.stopScan}
	return nil
}

func (m *Model) stopScan() tea.Cmd {
	if !m.scanRunning() {
		return nil
	}
	m.scan.cancel()
	m.scan.mu.Lock()
	m.scan.snap.stopping = true
	m.scan.mu.Unlock()
	return m.notify(levelInfo, "pausing the %s scan (finishing the proxies in flight)…", m.scan.req.Mode)
}

// paletteScan runs ':scan [MODE [SELECTION] [restart]]' and ':scan stop'.
func (m *Model) paletteScan(args []string) tea.Cmd {
	if len(args) == 0 {
		m.active = indexOf(TabScans)
		return tea.Batch(m.scans.refresh(m), m.scans.openForm(m, scan.ModeQuick))
	}
	if args[0] == "stop" || args[0] == "pause" {
		return m.askStopScan()
	}
	sel, restart := "all", false
	for _, a := range args[1:] {
		switch {
		case a == "restart":
			restart = true
		case contains(scanSelections, a):
			sel = a
		default:
			return m.notify(levelErr, "usage: :scan MODE [all|alive|filter|selected] [restart] | :scan stop")
		}
	}
	req, err := m.scanRequest(args[0], sel, restart)
	if err != nil {
		return m.notify(levelErr, "%v", err)
	}
	return m.startScan(req)
}

// scanSelected full-scans the proxy selected in the Proxies tab.
func (m *Model) scanSelected() tea.Cmd {
	req, err := m.scanRequest(scan.ModeFull, "selected", false)
	if err != nil {
		return m.notify(levelWarn, "%v", err)
	}
	return m.startScan(req)
}

// waitScanOnExit lets a cancelled scan write its state (paused) before the
// program returns and the database closes.
func (m *Model) waitScanOnExit() {
	s := m.scan
	if s == nil {
		return
	}
	s.cancel()
	if s.wait(300 * time.Millisecond) {
		return
	}
	if m.opts.Notice != nil {
		m.opts.Notice("pausing the %s scan (keeping what it measured)…", s.req.Mode)
	}
	if !s.wait(2*time.Minute) && m.opts.Notice != nil {
		m.opts.Notice("the %s scan did not stop in time; it resumes from its last saved position", s.req.Mode)
	}
}
