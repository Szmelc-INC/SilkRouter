package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/history"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

// usageTracker records one proxied command in the usage log: the row is
// started when the proxy is chosen, the proxy is measured while (or after)
// the command runs, and the row, the proxy's counters, its tags and the text
// log are updated when it exits. Every step is best effort: tracking never
// changes what the command does or its exit code.
type usageTracker struct {
	ctx     *cli.Context
	d       *db.DB
	own     bool // d was opened only for tracking
	pr      probe.Probe
	timeout time.Duration
	args    []string
	cmd     string
	mode    string
	measure string // parallel | after | off

	mu       sync.Mutex
	id       int64
	proxyID  int64
	proxyURL string
	chosen   proxy.Proxy
	verified map[string]probe.Result
	result   chan probe.Result
	started  time.Time
}

// newUsageTracker returns nil when the usage log is off or the database
// cannot be opened. d may be nil (it is then opened, and closed by finish).
func newUsageTracker(ctx *cli.Context, d *db.DB, pr probe.Probe, timeout time.Duration, args []string, mode string) *usageTracker {
	cfg := ctx.Config
	if !cfg.History.Record || len(args) == 0 {
		return nil
	}
	own := false
	if d == nil {
		if !cfg.DB.Record {
			return nil
		}
		var err error
		if d, err = openDB(ctx); err != nil {
			ctx.Debugf("usage log: %v", err)
			return nil
		}
		own = true
	}
	return &usageTracker{
		ctx: ctx, d: d, own: own, pr: pr, timeout: timeout, args: args, mode: mode,
		cmd: strings.TrimSuffix(filepath.Base(strings.ReplaceAll(args[0], `\`, "/")), ".exe"), measure: cfg.History.MeasureOnUse,
		verified: map[string]probe.Result{},
	}
}

func (t *usageTracker) bg() context.Context { return context.WithoutCancel(t.ctx) }

// onVerify remembers verification results (the chosen proxy's becomes the
// preflight of the usage row).
func (t *usageTracker) onVerify(p proxy.Proxy, r probe.Result) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.verified[p.Key()] = r
	t.mu.Unlock()
}

// onChosen starts the usage row and, in parallel mode, the measurement.
func (t *usageTracker) onChosen(p proxy.Proxy) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chosen, t.started = p, time.Now()
	ctx := t.bg()
	a := db.AddrOf(p)
	if row, err := t.d.GetProxyByAddr(ctx, a); err == nil {
		t.proxyID = row.ID
	} else if id, err := t.d.UpsertProxy(ctx, a, "route"); err == nil {
		t.proxyID = id
	}
	t.proxyURL = p.Key()
	cwd, _ := os.Getwd()
	u := db.Usage{
		TSStart: t.started.UTC(), ProxyID: t.proxyID, ProxyURL: t.proxyURL, Mode: t.mode, Argv: t.args, Cmd: t.cmd,
		Cwd: cwd, TTY: history.TTY(), PID: os.Getpid(), PPID: os.Getppid(), SessionID: os.Getenv("SILKROUTER_SESSION"),
	}
	if r, ok := t.verified[p.Key()]; ok {
		okv, ms := r.OK, r.LatencyMS
		u.PreflightOK, u.PreflightMS = &okv, &ms
	}
	id, err := t.d.StartUsage(ctx, u)
	if err != nil {
		t.ctx.Debugf("usage log: %v", err)
		return
	}
	t.id = id
	if _, verified := t.verified[p.Key()]; verified || t.measure != "parallel" {
		return
	}
	t.result = make(chan probe.Result, 1)
	go func() { t.result <- t.pr.Measure(ctx, p, 1, t.timeout) }()
}

// finish completes the row after the command exited with code.
func (t *usageTracker) finish(code int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.own {
		defer t.d.Close()
	}
	if t.id == 0 {
		return
	}
	ctx := t.bg()
	cfg := t.ctx.Config
	end := time.Now().UTC()
	var res *probe.Result
	switch {
	case t.result != nil:
		select {
		case r := <-t.result:
			res = &r
		case <-time.After(min(t.timeout, 3*time.Second)):
			// Never hold the shell long after the command is done: a probe
			// still running this late is dropped.
		}
	case t.measure == "after":
		if _, verified := t.verified[t.chosen.Key()]; !verified {
			r := t.pr.Measure(ctx, t.chosen, 1, t.timeout)
			res = &r
		}
	}
	ms := 0
	if r, ok := t.verified[t.chosen.Key()]; ok && r.OK {
		ms = r.LatencyMS
	}
	if res != nil {
		if res.OK {
			ms = res.LatencyMS
		}
		if _, err := t.d.RecordMeasurement(ctx, db.Measurement{ProxyID: t.proxyID, TS: end, Kind: db.KindRoute,
			Probe: t.pr.Name, OK: res.OK, MS: res.LatencyMS, ErrClass: probe.ClassifyErr(res.Err), ExitIP: res.ExitIP,
			Detail: `{"cmd":` + quoteJSON(t.cmd) + `}`}); err != nil {
			t.ctx.Debugf("usage log: %v", err)
		}
	}
	errClass := history.ClassifyExit(t.cmd, code)
	if err := t.d.FinishUsageWith(ctx, t.id, db.UsageEnd{ExitCode: code, ErrClass: errClass, End: end, MS: ms}); err != nil {
		t.ctx.Debugf("usage log: %v", err)
		return
	}
	if cfg.History.TextLog {
		c := code
		u := db.Usage{TSStart: t.started.UTC(), Mode: t.mode, ProxyURL: t.proxyURL, Argv: t.args, ExitCode: &c,
			DurationMS: end.Sub(t.started).Milliseconds(), MS: ms, ErrClass: errClass, TTY: history.TTY(),
			SessionID: os.Getenv("SILKROUTER_SESSION")}
		u.Cwd, _ = os.Getwd()
		if r, ok := t.verified[t.chosen.Key()]; ok {
			okv := r.OK
			u.PreflightOK = &okv
		}
		if err := history.AppendTextLog(filepath.Join(paths.LogsDir(), "usage.log"), u); err != nil {
			t.ctx.Debugf("usage log: %v", err)
		}
	}
	if cfg.Tags.Auto && t.proxyID > 0 {
		if err := score.RefreshFor(ctx, t.d, tagOptions(cfg), t.proxyID, t.cmd); err != nil {
			t.ctx.Debugf("tags: %v", err)
		}
	}
	maybePrune(ctx, t.ctx, t.d)
}

// maybePrune applies history.retention at most once a day.
func maybePrune(ctx context.Context, cctx *cli.Context, d *db.DB) {
	ret := cctx.Config.History.Retention.D()
	if ret <= 0 {
		return
	}
	const key = "last_prune_at"
	if v, ok, _ := d.GetMeta(ctx, key); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil && time.Since(t) < 24*time.Hour {
			return
		}
	}
	if n, err := d.PruneUsage(ctx, time.Now().Add(-ret)); err == nil {
		_ = d.SetMeta(ctx, key, time.Now().UTC().Format(time.RFC3339))
		if n > 0 {
			cctx.Debugf("usage log: pruned %d rows older than %s", n, cctx.Config.History.Retention)
		}
	}
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20:
			b.WriteString(" ")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
