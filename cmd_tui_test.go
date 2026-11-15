package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/scan"
	"github.com/szmelc-inc/silkrouter/internal/tui"
)

func TestTUICommand(t *testing.T) {
	sandbox(t)
	code, out, _ := runApp(t, "px", "tui", "--help")
	for _, want := range []string{"px tui [options] [search...]", "aliases: ui", "--no-alt-screen", "--no-mouse",
		"--refresh DUR", "--theme NAME", "-t, --tab TAB", "tui.start_tab", "tui.alt_screen", "-S, --search TEXT",
		"command palette"} {
		if !strings.Contains(out, want) {
			t.Errorf("tui --help misses %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatalf("tui --help: exit %d", code)
	}
	if _, out, _ = runApp(t, "px", "help", "ui"); !strings.Contains(out, "px tui") {
		t.Fatalf("help ui:\n%s", out)
	}

	// Bad flags are usage errors, checked before anything else.
	for _, args := range [][]string{{"--tab", "nope"}, {"--theme", "plaid"}, {"--sort", "speed"}, {"--status", "zombie"}} {
		code, _, errs := runApp(t, append([]string{"px", "ui"}, args...)...)
		if code != 2 || !strings.Contains(errs, "unknown") {
			t.Errorf("%v: exit %d %s", args, code, errs)
		}
	}

	// Without a terminal it explains instead of hanging.
	code, _, errs := runApp(t, "px", "tui", "--tab", "proxies")
	if code != 1 || !strings.Contains(errs, "interactive terminal") {
		t.Fatalf("no terminal: exit %d %s", code, errs)
	}
}

// TestTUIScanner runs the TUI's scanner (px scan's job runner) for real:
// a quick scan of proxies on closed local ports, no network.
func TestTUIScanner(t *testing.T) {
	sandbox(t)
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var ids []int64
	for i := 0; i < 3; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		a, _ := db.ParseAddr("http://" + ln.Addr().String())
		ln.Close()
		id, err := d.UpsertProxy(ctx, a, "test")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	cfg := config.Default()
	cfg.Scan.Timeout = config.Seconds(2)
	cfg.Scan.QuickWorkers = 4

	var mu sync.Mutex
	var started, reports int
	var run int64
	progress := func(p tui.ScanProgress) {
		mu.Lock()
		defer mu.Unlock()
		if p.Started {
			started++
			run = p.Run
		}
		if p.Report != nil {
			reports++
		}
	}
	scanner := tuiScanner(d)
	sum, err := scanner(ctx, cfg, tui.ScanRequest{Mode: scan.ModeQuick, Filter: db.Filter{Search: "127.0.0.1"}}, progress)
	if err != nil {
		t.Fatal(err)
	}
	if started != 1 || reports != 3 || run == 0 || sum.Run != run || sum.Done != 3 || sum.Dead != 3 || sum.State != db.ScanDone {
		t.Fatalf("started %d, reports %d, run %d, summary %+v", started, reports, run, sum)
	}
	r, err := d.GetScanRun(ctx, run)
	if err != nil || r.State != db.ScanDone || r.Done != 3 {
		t.Fatalf("scan run %+v %v", r, err)
	}
	for _, id := range ids {
		if p, _ := d.GetProxy(ctx, id); p.Status != db.StatusDead {
			t.Fatalf("#%d: %s", id, p.Status)
		}
	}

	// An empty selection is tui.ErrNoMatch.
	if _, err := scanner(ctx, cfg, tui.ScanRequest{Mode: scan.ModeQuick, Filter: db.Filter{Search: "nothing-here"}},
		progress); !errors.Is(err, tui.ErrNoMatch) {
		t.Fatalf("empty selection: %v", err)
	}
}
