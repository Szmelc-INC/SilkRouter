package tui

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/scan"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

// closedPort is a 127.0.0.1 address nothing listens on: checks through it
// fail at once, without any real network.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// engineScanner is a ScanFunc on the real scan engine in quick mode (what
// px scan's job runner does, minus lists and ranking): scan_runs row,
// engine, writer, progress for every report.
func engineScanner(d *db.DB) ScanFunc {
	return func(ctx context.Context, _ *config.Config, req ScanRequest, progress func(ScanProgress)) (ScanSummary, error) {
		f := req.Filter
		f.Sort = "id"
		rows, err := d.ListProxies(ctx, f)
		if err != nil {
			return ScanSummary{}, err
		}
		if len(rows) == 0 {
			return ScanSummary{Mode: req.Mode}, ErrNoMatch
		}
		bg := context.WithoutCancel(ctx)
		id, err := d.CreateScanRun(ctx, db.ScanRun{Mode: req.Mode, Total: len(rows)})
		if err != nil {
			return ScanSummary{}, err
		}
		progress(ScanProgress{Run: id, Started: true, Total: len(rows), Extras: " (test engine)"})
		progress(ScanProgress{Message: "lists: not used by quick scans"})
		targets := make([]scan.Target, len(rows))
		for i, p := range rows {
			targets[i] = scan.Target{ID: p.ID, Proxy: p.ToProxy()}
		}
		cf, _ := probe.Get("cloudflare")
		w := d.NewWriter(ctx, db.WriterOptions{})
		done := 0
		eng := scan.New(scan.Options{Mode: scan.ModeQuick, Workers: 4, Tries: 1, Timeout: 2 * time.Second,
			Probes: []probe.Probe{cf}, OnDone: func(r scan.Report) {
				done++
				progress(ScanProgress{Report: &r, Done: done, Total: len(rows)})
			}}, w)
		runErr := eng.Run(ctx, targets)
		if err := w.Close(); err != nil {
			return ScanSummary{}, err
		}
		state := db.ScanDone
		if runErr != nil {
			state = db.ScanPaused
		}
		if err := d.UpdateScanRun(bg, id, done, "", state, 0); err != nil {
			return ScanSummary{}, err
		}
		st := eng.Stats()
		return ScanSummary{Run: id, Mode: req.Mode, State: state, Total: len(rows), Done: done, Alive: st.Alive,
			Dead: st.Dead, Elapsed: time.Since(st.Started), Interrupted: runErr != nil}, nil
	}
}

// scripted is a ScanFunc the test steers: it reports one alive proxy, then
// waits for go (finishing) or for cancellation (pausing).
type scripted struct {
	mu      sync.Mutex
	reqs    []ScanRequest
	reached chan struct{} // one report is out
	goOn    chan struct{}
	block   bool
}

func newScripted(block bool) *scripted {
	return &scripted{reached: make(chan struct{}, 8), goOn: make(chan struct{}, 8), block: block}
}

func (s *scripted) scan(ctx context.Context, _ *config.Config, req ScanRequest, progress func(ScanProgress)) (ScanSummary, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	sum := ScanSummary{Run: 99, Mode: req.Mode, Total: 3}
	progress(ScanProgress{Run: 99, Started: true, Total: 3, Extras: " (scripted)"})
	progress(ScanProgress{Report: &scan.Report{ID: 1, URL: "socks5://192.0.2.9:1080", Alive: true, MS: 77, Country: "DE",
		Anonymity: "elite"}, Done: 1, Total: 3})
	s.reached <- struct{}{}
	if s.block {
		select {
		case <-s.goOn:
		case <-ctx.Done():
			sum.Done, sum.State, sum.Interrupted, sum.Alive = 1, db.ScanPaused, true, 1
			return sum, nil
		}
	}
	for i := 2; i <= 3; i++ {
		progress(ScanProgress{Report: &scan.Report{ID: int64(i), URL: "http://192.0.2.10:80", ErrClass: "refused"}, Done: i, Total: 3})
	}
	sum.Done, sum.State, sum.Alive, sum.Dead = 3, db.ScanDone, 1, 2
	return sum, nil
}

func (s *scripted) requests() []ScanRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ScanRequest(nil), s.reqs...)
}

func (s *scripted) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-s.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the scan never reported")
	}
}

// waitScan polls the running scan (as the program's timer does) until its
// end was shown.
func (h *harness) waitScan() {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for h.m.scan == nil || !h.m.scan.reported {
		if time.Now().After(deadline) {
			h.t.Fatal("the scan did not finish")
		}
		time.Sleep(10 * time.Millisecond)
		h.send(scanPollMsg{})
	}
}

func TestScanFromTheTUI(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.StartScan = engineScanner(o.DB) })
	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, addProxy(t, h.d, "http://"+closedPort(t)))
	}
	h.selectProxy("127.0.0.1") // the Proxies filter: the three fake proxies

	h.key("5", "n")
	h.mustSee("New scan", "mode", "‹ quick ›", "selection", "‹ all ›", "8 proxies") // 5 seeded + 3
	h.key("f")
	h.mustSee("‹ filter ›", "3 proxies", "the proxy filter (status:all · /127.0.0.1)")
	h.key("down", "down", "right") // restart: yes
	h.mustSee("‹ yes ›", "start over")
	h.key("x") // back to no
	h.key("enter")
	if h.m.scans.form != nil {
		t.Fatal("enter did not start the scan")
	}
	h.waitScan()
	h.mustSee("QUICK scan #2", "done", "3/3", "0 alive", "3 dead", "lists: not used by quick scans")
	h.mustSee("quick scan #2 done: 3/3")
	if len(h.m.jobs) != 0 {
		t.Fatalf("the scan job is still registered: %v", h.m.jobs)
	}
	// The runs table holds the finished run.
	if len(h.m.scans.runs) < 2 || h.m.scans.runs[0].ID != 2 || h.m.scans.runs[0].State != db.ScanDone {
		t.Fatalf("runs table: %+v", h.m.scans.runs)
	}
	for _, id := range ids {
		p, _ := h.d.GetProxy(context.Background(), id)
		ms, _ := h.d.Measurements(context.Background(), id, 5)
		if p.Status != db.StatusDead || len(ms) == 0 || ms[0].Kind != db.KindScan {
			t.Fatalf("proxy #%d after the scan: %s %v", id, p.Status, ms)
		}
	}
	// esc hides the finished panel.
	h.key("esc")
	h.mustNotSee("QUICK scan #2")
	h.mustSee("quick", "3/3")

	// Nothing to scan: the form refuses, the palette reports it.
	h.palette("scan quick filter")
	h.waitScan()
	if h.m.scan.snapshot().err != nil {
		t.Fatalf("second scan: %v", h.m.scan.snapshot().err)
	}
	h.key("2", "/", "ctrl+u")
	h.typ("no-such-proxy")
	h.key("enter")
	h.palette("scan quick filter")
	h.waitScan()
	h.mustSee("nothing to scan")
}

func TestScanProgressAndOneAtATime(t *testing.T) {
	sc := newScripted(true)
	h := newHarness(t, func(o *Options) { o.StartScan = sc.scan })
	h.palette("scan quick alive")
	sc.waitReached(t)
	h.send(scanPollMsg{})
	h.mustSee("scan quick 1/3") // the status bar's job, from any tab
	h.key("5")
	h.mustSee("QUICK scan #99", "alive proxies", "running", "1/3", "1 alive", "0 dead", "last socks5://192.0.2.9:1080",
		"alive so far (1)", "socks5://192.0.2.9:1080", "77", "elite")
	if r := sc.requests(); len(r) != 1 || r[0].Mode != scan.ModeQuick || r[0].Selection != "alive" ||
		len(r[0].Filter.Status) != 1 || r[0].Filter.Status[0] != db.StatusAlive {
		t.Fatalf("request %+v", r)
	}

	// One scan at a time.
	h.palette("scan full")
	h.mustSee("one at a time")
	h.key("n")
	h.mustSee("x pauses it first")
	if len(sc.requests()) != 1 || h.m.scans.form != nil {
		t.Fatal("a second scan started")
	}

	sc.goOn <- struct{}{}
	h.waitScan()
	h.mustSee("QUICK scan #99", "done", "3/3", "1 alive", "2 dead", "quick scan #99 done: 3/3")
}

func TestScanPauseAndQuit(t *testing.T) {
	sc := newScripted(true)
	h := newHarness(t, func(o *Options) { o.StartScan = sc.scan })
	h.key("5")
	h.palette("scan reputation")
	sc.waitReached(t)
	h.send(scanPollMsg{})

	h.key("x")
	h.mustSee("pause the reputation scan #99 at 1/3?")
	h.key("n")
	if !h.m.scanRunning() {
		t.Fatal("n paused the scan")
	}
	h.palette("scan stop")
	h.mustSee("pause the reputation scan")
	h.key("y")
	h.mustSee("pausing the reputation scan")
	h.waitScan()
	h.mustSee("paused", "reputation scan #99 paused at 1/3")
	if sn := h.m.scan.snapshot(); !sn.sum.Interrupted {
		t.Fatalf("not interrupted: %+v", sn.sum)
	}

	// Quitting while a scan runs asks, then cancels it (it pauses).
	h.palette("scan quick restart")
	sc.waitReached(t)
	if r := sc.requests(); !r[len(r)-1].Restart || r[len(r)-1].Selection != "all" {
		t.Fatalf("request %+v", r[len(r)-1])
	}
	h.key("q")
	h.mustSee("quit anyway? The scan pauses")
	h.key("y")
	if !h.quit {
		t.Fatal("y did not quit")
	}
	h.m.waitScanOnExit() // what Run does before returning
	if sn := h.m.scan.snapshot(); !sn.finished || !sn.sum.Interrupted {
		t.Fatalf("the scan did not pause on quit: %+v", sn)
	}
}

// Pausing while the scan still prepares (lists, real IP) ends it with
// context.Canceled: shown as stopped, not as a failure.
func TestScanStoppedBeforeStart(t *testing.T) {
	entered := make(chan struct{}, 1)
	h := newHarness(t, func(o *Options) {
		o.StartScan = func(ctx context.Context, _ *config.Config, _ ScanRequest, _ func(ScanProgress)) (ScanSummary, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return ScanSummary{}, ctx.Err()
		}
	})
	h.key("5")
	h.palette("scan full")
	<-entered
	h.mustSee("FULL scan", "starting…")
	h.key("x", "y")
	h.waitScan()
	h.mustSee("stopped", "full scan stopped before it started")
	h.mustNotSee("failed")
}

func TestScanSelectedProxy(t *testing.T) {
	sc := newScripted(false)
	h := newHarness(t, func(o *Options) { o.StartScan = sc.scan })
	h.selectProxy("192.0.2.2")
	h.key("F")
	sc.waitReached(t)
	h.waitScan()
	r := sc.requests()
	if len(r) != 1 || r[0].Mode != scan.ModeFull || r[0].Selection != "selected" || len(r[0].Filter.IDs) != 1 ||
		r[0].Filter.IDs[0] != h.ids["slow"] {
		t.Fatalf("F request %+v", r)
	}
	h.mustSee("full scan #99 done")

	// From the details too, and with the form's "selected" choice.
	h.key("enter", "F")
	sc.waitReached(t)
	h.waitScan()
	h.key("5", "n", "s", "2")
	h.mustSee("‹ full ›", "‹ selected ›", "1 proxy ·", "proxy #2 "+slowURL)
	h.key("esc")

	// Without a scanner nothing starts.
	h2 := newHarness(t)
	h2.key("5", "n")
	h2.mustSee("cannot be started")
	h2.palette("scan quick")
	h2.mustSee("cannot be started")
}

func TestScanPaletteCompletion(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.StartScan = newScripted(false).scan })
	h.key(":")
	h.typ("scan ")
	for _, want := range append(append([]string(nil), scan.Modes...), "stop") {
		if !contains(h.m.pal.cands, want) {
			t.Fatalf("candidates %v lack %q", h.m.pal.cands, want)
		}
	}
	h.typ("quick ")
	if !contains(h.m.pal.cands, "selected") || !contains(h.m.pal.cands, "restart") {
		t.Fatalf("candidates %v", h.m.pal.cands)
	}
	h.key("esc")
	h.palette("scan")
	if h.m.activeView() != view(h.m.scans) || h.m.scans.form == nil {
		t.Fatal(":scan alone did not open the form")
	}
	h.key("esc")
	h.palette("scan bogus")
	h.mustSee("unknown scan mode")
	h.palette("scan quick sideways")
	h.mustSee("usage: :scan")
}

func TestMITMShown(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Color = "always" })
	ctx := context.Background()
	id := h.ids["fast"]
	if err := h.d.SetInfo(ctx, db.ProxyInfo{ProxyID: id, TLSMITM: db.Bool(true), TLSIssuer: "Evil Corp Inspection CA",
		HTTPSOK: db.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := score.RefreshTags(ctx, h.d, score.TagOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.tags("fast"), " mitm ") {
		t.Fatalf("no mitm tag: %s", h.tags("fast"))
	}
	h.palette("set tui.columns id,proxy,tls,tags")
	h.selectProxy("192.0.2.1")
	h.mustSee("TLS", "MITM", "mitm")
	if c := h.m.prox.tbl.rows[0][2]; c.text != "MITM" {
		t.Fatalf("tls cell %q", c.text)
	}
	h.key("enter")
	h.mustSee("MITM", "https / tls", "INTERCEPTED (it decrypts HTTPS)", "tls issuer", "Evil Corp Inspection CA")
	if red := h.m.styles.tagStyle("mitm").Render("mitm"); !strings.Contains(h.m.View(), red) || red == "mitm" {
		t.Fatal("the mitm tag is not drawn in its (red) style")
	}

	// The other verdicts.
	h.key("esc")
	h.selectProxy("192.0.2.2")
	h.key("enter")
	h.mustSee("not checked (F: full scan)")
	if err := h.d.SetInfo(ctx, db.ProxyInfo{ProxyID: h.ids["slow"], HTTPSOK: db.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	h.key("r")
	h.mustSee("certificate verified")
	h.key("esc", "ctrl+r")
	if c := h.m.prox.tbl.rows[0][2]; c.text != "ok" {
		t.Fatalf("tls cell %q", c.text)
	}
}

func TestScanPanelFitsSmallTerminals(t *testing.T) {
	sc := newScripted(true)
	h := newHarness(t, func(o *Options) { o.StartScan = sc.scan; o.Color = "always" })
	h.palette("scan full")
	sc.waitReached(t)
	h.send(scanPollMsg{})
	h.key("5")
	for _, size := range [][2]int{{150, 40}, {80, 24}, {50, 12}} {
		h.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, form := range []bool{false, true} {
			if form {
				h.m.scans.form = &scanForm{mode: scan.ModeFull, sel: "all", count: 12}
			}
			lines := strings.Split(h.m.View(), "\n")
			if len(lines) != size[1] {
				t.Fatalf("%v form=%v: %d lines", size, form, len(lines))
			}
			h.m.scans.form = nil
		}
	}
	sc.goOn <- struct{}{}
	h.waitScan()
}
