package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/reputation"
	"github.com/szmelc-inc/silkrouter/internal/scan"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

// scanJob is one scan run, independent of what drives it (px scan, the
// TUI). Every callback is optional and called from one goroutine at a time.
type scanJob struct {
	Mode    string
	Filter  db.Filter // selection; Sort / Limit / Offset / AfterID are ignored
	Limit   int       // at most N targets (0 = all)
	Restart bool      // ignore a resumable run of the same selection

	Workers, Tries int
	Timeout, Delay time.Duration
	Probes         []probe.Probe

	NoDNSBL, NoLists, NoGeo, NoProto, NoJudge, NoHTTPS bool
	MXToolbox, WIMIA, Offline                          bool

	Rank           string
	NoRank, NoTags bool

	Logf     func(format string, a ...any) // progress messages ("lists: 20/20 loaded")
	Debugf   func(format string, a ...any)
	OnStart  func(run *db.ScanRun, total, done int, extras string)
	OnReport func(r scan.Report, done, total int)
}

// scanResult summarises a finished (or paused) scan job.
type scanResult struct {
	Run         int64          `json:"run"`
	Mode        string         `json:"mode"`
	State       string         `json:"state"`
	Total       int            `json:"total"`
	Done        int            `json:"done"`
	Alive       int64          `json:"alive"`
	Dead        int64          `json:"dead"`
	Listed      int64          `json:"listed"`
	ElapsedNS   time.Duration  `json:"elapsed_ns"`
	Writer      db.WriterStats `json:"writer"`
	Ranked      int            `json:"ranked"`
	Tags        map[string]int `json:"tags,omitempty"`
	Resumed     bool           `json:"resumed,omitempty"`
	Interrupted bool           `json:"interrupted,omitempty"`
	AliveIDs    []int64        `json:"-"`
}

// errNoMatch: the selection matched no proxy (and nothing was resumable).
var errNoMatch = errors.New("no proxies match")

// defaultScanJob is a job with every knob at its config default.
func defaultScanJob(cfg *config.Config, mode string) (scanJob, error) {
	j := scanJob{
		Mode: mode, Workers: cfg.Scan.Workers, Tries: cfg.Scan.Tries, Timeout: cfg.Scan.Timeout.D(),
		Delay: cfg.Scan.Delay.D(), Restart: !cfg.Scan.Resume, NoDNSBL: !cfg.Scan.DNSBL, NoLists: !cfg.Scan.Lists,
		NoGeo: cfg.Scan.Geo == "none", NoProto: !cfg.Scan.Protocols, NoJudge: cfg.Scan.JudgeURL == "",
		NoHTTPS: cfg.Scan.HTTPSURL == "", MXToolbox: cfg.Scan.MXToolbox, WIMIA: cfg.Scan.WIMIA,
		Rank: cfg.Scan.Rank, NoTags: !cfg.Tags.Auto,
	}
	if mode == scan.ModeQuick {
		j.Workers = cfg.Scan.QuickWorkers
	}
	var err error
	j.Probes, err = parseProbes(strings.Join(cfg.Scan.Probes, ","))
	return j, err
}

func parseProbes(list string) ([]probe.Probe, error) {
	var out []probe.Probe
	for _, n := range strings.Split(list, ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		p, err := probe.Get(n)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// selectionKey identifies a selection for resuming: the same mode and
// filter resume the same run (FullScanBefore only counts as set / unset,
// since --older-than moves with the clock).
func selectionKey(mode string, f db.Filter) string {
	k := f
	k.Sort, k.Reverse, k.Limit, k.Offset, k.AfterID, k.WithTags = "", false, 0, 0, 0, false
	older := !k.FullScanBefore.IsZero()
	k.FullScanBefore = time.Time{}
	b, _ := json.Marshal(k)
	return fmt.Sprintf("%s|%s|older=%v", mode, b, older)
}

// runScanJob runs j against d: resume or create the scan_runs row, build
// the engine (lists, DNSBL, geo, judge), scan, keep the cursor, then
// refresh listings, scores, ranks and tags. ctx cancellation pauses the
// run; everything gathered is kept.
func runScanJob(ctx context.Context, d *db.DB, cfg *config.Config, j scanJob) (scanResult, error) {
	logf := func(format string, a ...any) {
		if j.Logf != nil {
			j.Logf(format, a...)
		}
	}
	debugf := func(format string, a ...any) {
		if j.Debugf != nil {
			j.Debugf(format, a...)
		}
	}
	bg := context.WithoutCancel(ctx)
	res := scanResult{Mode: j.Mode}
	if len(j.Probes) == 0 && (j.Mode == scan.ModeFull || j.Mode == scan.ModeQuick) {
		return res, errors.New("no probes (--probes / scan.probes)")
	}
	f := j.Filter
	optsJSON, _ := json.Marshal(scanRunOpts{Mode: j.Mode, Key: selectionKey(j.Mode, f)})
	var run *db.ScanRun
	if prev, err := d.ResumableScanRun(ctx, j.Mode); err == nil {
		if !j.Restart && prev.Opts == string(optsJSON) {
			run = prev
			res.Resumed = true
			if c, err := strconv.ParseInt(prev.Cursor, 10, 64); err == nil && c > 0 {
				f.AfterID = c
			}
			logf("resuming %s scan #%d at %d/%d (--restart starts over)", j.Mode, prev.ID, prev.Done, prev.Total)
		} else {
			state := db.ScanFailed
			if !j.Restart {
				logf("note: %s scan #%d (%d/%d) used other filters; starting a new one", j.Mode, prev.ID, prev.Done, prev.Total)
				state = db.ScanPaused
			}
			_ = d.UpdateScanRun(bg, prev.ID, prev.Done, prev.Cursor, state, 0)
		}
	}
	f.Sort, f.Reverse, f.Limit, f.Offset = "id", false, 0, 0
	rows, err := d.ListProxies(ctx, f)
	if err != nil {
		return res, err
	}
	if j.Limit > 0 && len(rows) > j.Limit {
		rows = rows[:j.Limit]
	}
	if len(rows) == 0 {
		if run != nil {
			_ = d.UpdateScanRun(bg, run.ID, run.Done, run.Cursor, db.ScanDone, 0)
			res.Run, res.State, res.Done, res.Total = run.ID, db.ScanDone, run.Done, run.Total
			logf("%s scan #%d was already complete", j.Mode, run.ID)
			return res, nil
		}
		return res, errNoMatch
	}
	targets := make([]scan.Target, len(rows))
	for i, p := range rows {
		targets[i] = scan.Target{ID: p.ID, Proxy: p.ToProxy(), ExitIP: p.ExitIP}
	}
	done0 := 0
	if run == nil {
		id, err := d.CreateScanRun(ctx, db.ScanRun{Mode: j.Mode, Total: len(targets), Opts: string(optsJSON)})
		if err != nil {
			return res, err
		}
		run = &db.ScanRun{ID: id, Mode: j.Mode, Total: len(targets)}
	} else {
		done0 = run.Done
		_ = d.UpdateScanRun(bg, run.ID, run.Done, run.Cursor, db.ScanRunning, 0)
	}
	res.Run, res.Total = run.ID, len(targets)+done0

	eo, err := engineOptions(ctx, cfg, j, logf, debugf)
	if err != nil {
		return res, err
	}
	w := d.NewWriter(ctx, db.WriterOptions{
		BatchSize: cfg.DB.BatchSize, FlushInterval: cfg.DB.FlushInterval.D(),
		OnError: func(err error, n int) { logf("warning: database: %d rows not recorded: %v", n, err) },
	})

	// Cursor: the largest id such that every target up to it is done.
	ids := make([]int64, len(targets))
	for i, t := range targets {
		ids[i] = t.ID
	}
	var mu sync.Mutex
	doneSet := map[int64]bool{}
	next := 0
	cursor := run.Cursor
	doneN := done0
	lastSave := time.Now()
	eo.OnDone = func(r scan.Report) {
		mu.Lock()
		defer mu.Unlock()
		doneSet[r.ID] = true
		doneN++
		for next < len(ids) && doneSet[ids[next]] {
			next++
		}
		if next > 0 {
			cursor = strconv.FormatInt(ids[next-1], 10)
		}
		if r.Alive {
			res.AliveIDs = append(res.AliveIDs, r.ID)
		}
		if j.OnReport != nil {
			j.OnReport(r, doneN, res.Total)
		}
		if time.Since(lastSave) > 2*time.Second {
			lastSave = time.Now()
			_ = d.UpdateScanRun(bg, run.ID, doneN, cursor, db.ScanRunning, 0)
		}
	}
	eng := scan.New(eo, w)
	if j.OnStart != nil {
		j.OnStart(run, res.Total, done0, scanExtras(eo))
	}
	runErr := eng.Run(ctx, targets)
	if err := w.Close(); err != nil {
		logf("warning: database: %v", err)
	}
	res.Interrupted = runErr != nil
	res.State = db.ScanDone
	if res.Interrupted {
		res.State = db.ScanPaused
	}
	mu.Lock()
	_ = d.UpdateScanRun(bg, run.ID, doneN, cursor, res.State, 0)
	res.Done = doneN
	mu.Unlock()
	_ = d.RefreshListed(bg)

	st := eng.Stats()
	res.Alive, res.Dead, res.Listed, res.ElapsedNS, res.Writer = st.Alive, st.Dead, st.Listed, time.Since(st.Started), w.Stats()
	if !j.NoRank {
		rr, err := score.Compute(bg, d, scoreOptions(cfg, j.Rank))
		if err != nil {
			logf("warning: ranking: %v", err)
		}
		res.Ranked = rr.Ranked
	}
	if !j.NoTags {
		tr, err := score.RefreshTags(bg, d, tagOptions(cfg))
		if err != nil {
			logf("warning: tags: %v", err)
		}
		res.Tags = tr.Counts
	}
	return res, nil
}

// engineOptions builds the scan engine's options for j: probes, the exit /
// HTTPS / judge checks, lists, DNS blacklists, geolocation, mxtoolbox, wimia.
func engineOptions(ctx context.Context, cfg *config.Config, j scanJob, logf, debugf func(string, ...any)) (scan.Options, error) {
	eo := scan.Options{
		Mode: j.Mode, Workers: j.Workers, Tries: j.Tries, Timeout: j.Timeout, Delay: j.Delay,
		Probes: j.Probes, FailFast: cfg.Scan.FailFast, Protocols: !j.NoProto,
	}
	if j.Mode == scan.ModeQuick {
		eo.Delay = 0
	}
	if j.Mode == scan.ModeFull {
		if cfg.Scan.ExitProbe != "" {
			if p, err := probe.Get(cfg.Scan.ExitProbe); err == nil {
				eo.ExitProbe = &p
			}
		}
		if !j.NoHTTPS {
			eo.HTTPSURLs = httpsURLs(cfg)
		}
		if !j.NoJudge && cfg.Scan.JudgeURL != "" {
			eo.JudgeURL = cfg.Scan.JudgeURL
			if cfg.Scan.RealIPURL != "" {
				ip, err := scan.RealIP(ctx, cfg.Scan.RealIPURL, j.Timeout)
				if err != nil {
					logf("warning: cannot learn your real IP (%v): transparent proxies look anonymous", err)
				} else {
					eo.RealIP = ip
					debugf("real IP: %s", ip)
				}
			}
		}
		if j.WIMIA {
			eo.WIMIA = &scan.PXChecker{Timeout: j.Timeout}
		}
	}
	if j.Mode == scan.ModeFull || j.Mode == scan.ModeReputation {
		if !j.NoLists {
			m, err := listManager(cfg)
			if err != nil {
				return eo, err
			}
			results := m.Load(ctx, !j.Offline, false)
			eo.Lists = reputation.Sets(results)
			var bad []string
			for _, r := range results {
				if r.Set == nil {
					bad = append(bad, r.Meta.Name)
				}
			}
			msg := fmt.Sprintf("lists: %d/%d loaded", len(eo.Lists), len(results))
			if len(bad) > 0 {
				msg += " (unavailable: " + strings.Join(bad, ", ") + ")"
			}
			logf("%s", msg)
		}
		if !j.NoDNSBL && len(cfg.Scan.DNSBLZones) > 0 {
			eo.DNSBL = &reputation.DNSBL{Zones: cfg.Scan.DNSBLZones, Timeout: cfg.Scan.DNSTimeout.D()}
		}
		if j.MXToolbox {
			chk := scan.NewBLChecker(cfg.BLCheck.Token, cfg.BLCheck.Timeout.D())
			if err := chk.EnsureToken(ctx); err != nil {
				logf("warning: mxtoolbox disabled: %v", err)
			} else {
				eo.MXTB, eo.MXTBDelay = chk, cfg.BLCheck.Delay.D()
			}
		}
	}
	if (j.Mode == scan.ModeFull || j.Mode == scan.ModeGeo) && !j.NoGeo && cfg.Scan.Geo == "ip-api" {
		eo.Geo = &reputation.GeoClient{URL: cfg.Scan.GeoURL}
	}
	return eo, nil
}

// httpsURLs is the primary HTTPS check URL plus the fallbacks.
func httpsURLs(cfg *config.Config) []string {
	var out []string
	for _, u := range append([]string{cfg.Scan.HTTPSURL}, cfg.Scan.HTTPSFallbacks...) {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}
