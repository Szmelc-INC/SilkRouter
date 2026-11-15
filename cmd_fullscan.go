package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/reputation"
	"github.com/szmelc-inc/silkrouter/internal/scan"
	"github.com/szmelc-inc/silkrouter/internal/score"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

var scanOpts struct {
	filter     filterOpts
	workers    int
	tries      int
	timeout    config.Duration
	delay      config.Duration
	probes     string
	noDNSBL    bool
	noLists    bool
	noGeo      bool
	noProto    bool
	noJudge    bool
	noHTTPS    bool
	mxtoolbox  bool
	wimia      bool
	restart    bool
	rank       string
	noRank     bool
	noTags     bool
	offline    bool
	unscanned  bool
	olderThan  config.Duration
	json       bool
	top        int
	showAlive  bool
	runsLimit  int
	listsForce bool
	listsJSON  bool
	checkDNSBL bool
}

func scanFlags(mode string) func(fs *cli.FlagSet) {
	return func(fs *cli.FlagSet) {
		cfg := fs.Config
		o := &scanOpts
		filterFlags(fs, &o.filter, false)
		workers, wkey := cfg.Scan.Workers, "scan.workers"
		if mode == scan.ModeQuick {
			workers, wkey = cfg.Scan.QuickWorkers, "scan.quick_workers"
		}
		fs.Int(&o.workers, "j,workers,jobs", workers, "proxies scanned in parallel").ConfigKey(wkey)
		fs.Int(&o.tries, "tries", cfg.Scan.Tries, "samples per probe").ConfigKey("scan.tries")
		fs.Duration(&o.timeout, "t,timeout", cfg.Scan.Timeout, "timeout of one check").ConfigKey("scan.timeout")
		fs.Duration(&o.delay, "delay", cfg.Scan.Delay, "pause between two checks of one proxy").ConfigKey("scan.delay")
		fs.String(&o.probes, "probes", strings.Join(cfg.Scan.Probes, ","), "probes to run (comma-separated; quick uses the first)").
			Placeholder("LIST").Dynamic("probes").ConfigKey("scan.probes")
		if mode == scan.ModeFull || mode == scan.ModeReputation {
			fs.Bool(&o.noDNSBL, "no-dnsbl", !cfg.Scan.DNSBL, "skip DNS blacklists").ConfigKey("scan.dnsbl")
			fs.Bool(&o.noLists, "no-lists", !cfg.Scan.Lists, "skip the known-proxy / tor / abuse lists").ConfigKey("scan.lists")
			fs.Bool(&o.offline, "offline", false, "never download lists (use the local copies)")
			fs.Bool(&o.mxtoolbox, "mxtoolbox", cfg.Scan.MXToolbox, "also query mxtoolbox (slow)").ConfigKey("scan.mxtoolbox")
		}
		if mode == scan.ModeFull || mode == scan.ModeGeo {
			fs.Bool(&o.noGeo, "no-geo", cfg.Scan.Geo == "none", "skip geolocation / ASN lookups").ConfigKey("scan.geo")
		}
		if mode == scan.ModeFull {
			fs.Bool(&o.noProto, "no-protocols", !cfg.Scan.Protocols, "skip protocol detection").ConfigKey("scan.protocols")
			fs.Bool(&o.noJudge, "no-judge", cfg.Scan.JudgeURL == "", "skip the anonymity judge").ConfigKey("scan.judge_url")
			fs.Bool(&o.noHTTPS, "no-https", cfg.Scan.HTTPSURL == "", "skip the HTTPS / TLS check").ConfigKey("scan.https_url")
			fs.Bool(&o.wimia, "wimia", cfg.Scan.WIMIA, "also run the whatismyipaddress check").ConfigKey("scan.wimia")
			fs.Bool(&o.unscanned, "unscanned", false, "only proxies never full-scanned")
			fs.Duration(&o.olderThan, "older-than", 0, "only proxies not full-scanned for this long")
		}
		fs.Bool(&o.restart, "restart", !cfg.Scan.Resume, "start over instead of resuming an interrupted scan").ConfigKey("scan.resume")
		fs.String(&o.rank, "rank", cfg.Scan.Rank, "rank order written afterwards").Placeholder("MODE").
			Choices(score.RankModes...).ConfigKey("scan.rank")
		fs.Bool(&o.noRank, "no-rank", false, "do not recompute scores and ranks afterwards")
		fs.Bool(&o.noTags, "no-tags", !cfg.Tags.Auto, "do not refresh heuristic tags afterwards").ConfigKey("tags.auto")
		fs.Int(&o.top, "top", 15, "show the N best proxies at the end (0 = none)")
		fs.Bool(&o.showAlive, "a,show-alive", false, "print every working proxy as it is found")
		fs.Bool(&o.json, "json", false, "print the summary as JSON")
	}
}

var scanModeDesc = map[string]string{
	scan.ModeQuick: `Tests every selected proxy once with the first probe (alive or dead, how
fast). Like recache, but over the whole database (any status) and as a
resumable scan run.`,
	scan.ModeFull: `Measures every selected proxy with every check, gently (few workers, a
pause between two checks of the same proxy), so you know everything about an
address before you tunnel through it:

  1. TCP connect time to the proxy
  2. every probe in scan.probes, scan.tries times each (median latency,
     jitter); dead proxies stop after scan.fail_fast failed probes
  3. exit IP (scan.exit_probe)
  4. HTTPS through the proxy (CONNECT + TLS); the Cloudflare trace also
     yields the exit country
  5. anonymity judge: transparent (your IP leaks), anonymous (proxy headers)
     or elite
  6. protocol detection: which of http / socks5 / socks4 the port speaks
     (new combinations are added to the database)
  7. reputation of host and exit IP: known-proxy, Tor, VPN, datacenter and
     abuse lists (px lists) and DNS blacklists (scan.dnsbl_zones)
  8. geolocation / ASN / hosting / mobile (ip-api, queried directly)
  9. optionally mxtoolbox (--mxtoolbox) and whatismyipaddress (--wimia)

Afterwards every proxy gets a speed, clean and stability score, alive ones a
rank (scan.rank: balanced, speed, clean or stable) and the heuristic tags are
refreshed. Ctrl-C pauses: everything measured so far is kept, and the next
'scan full' with the same filters resumes where it stopped (--restart starts
over).`,
	scan.ModeReputation: `Checks the host and the last known exit IP of every selected proxy against
the known-proxy / Tor / VPN / datacenter / abuse lists and the DNS
blacklists, without sending any traffic through the proxies.`,
	scan.ModeGeo:       `Looks up country, region, city, ASN, organisation and hosting / mobile flags of every selected proxy's exit IP (or host) with ip-api, in batches.`,
	scan.ModeProtocols: `Tries the http (CONNECT), socks5 and socks4 handshakes on every selected host:port and records which work; combinations not in the database yet are added.`,
}

func scanSub(mode, summary string, examples ...cli.Example) cli.Command {
	return cli.Command{
		Name:        mode,
		Summary:     summary,
		Description: scanModeDesc[mode],
		Usage:       "[filters] [options] [search...]",
		NeedsHome:   true,
		Flags:       scanFlags(mode),
		Examples:    examples,
		Run:         func(ctx *cli.Context, args []string) int { return runScan(ctx, mode, args) },
	}
}

func init() {
	cli.Register(cli.Command{
		Name:    "scan",
		Group:   cli.GroupScan,
		Summary: "scan the database: quick, full, reputation, geo, protocols",
		Description: `Scans walk the proxies of the database (filtered like 'px proxies') with a
pool of workers, record every result as facts, and can be interrupted and
resumed. 'scan full' runs every available check on every address; the other
modes run one family of checks. Results feed 'px proxies', 'px show',
'px rank', the tags and route's choice.`,
		Subcommands: []cli.Command{
			scanSub(scan.ModeFull, "every check on every proxy, gently, then rank them",
				cli.Example{Cmd: "scan full", Desc: "everything, every proxy (resumable)"},
				cli.Example{Cmd: "scan full --status alive", Desc: "only proxies that worked last time"},
				cli.Example{Cmd: "scan full --socks -j 64 --tries 2"},
				cli.Example{Cmd: "scan full --older-than 7d --rank clean"},
			),
			scanSub(scan.ModeQuick, "one probe per proxy: alive or dead, latency",
				cli.Example{Cmd: "scan quick", Desc: "re-test the whole database"},
				cli.Example{Cmd: "scan quick --status unknown -j 400"},
			),
			scanSub(scan.ModeReputation, "lists + DNS blacklists only (no proxy traffic)",
				cli.Example{Cmd: "scan reputation --status alive"},
			),
			scanSub(scan.ModeGeo, "geolocation / ASN of exit IPs only",
				cli.Example{Cmd: "scan geo --status alive"},
			),
			scanSub(scan.ModeProtocols, "detect which protocols each address speaks",
				cli.Example{Cmd: "scan protocols --status alive"},
			),
			{
				Name:      "runs",
				Aliases:   []string{"history", "ls"},
				Summary:   "list scan runs (progress, state, resumable)",
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.Int(&scanOpts.runsLimit, "n,limit", 20, "show at most N runs (0 = all)")
					fs.Bool(&scanOpts.json, "json", false, "print the runs as JSON")
				},
				Run: runScanRuns,
			},
		},
		Examples: []cli.Example{
			{Cmd: "scan full", Desc: "know everything about every address"},
			{Cmd: "scan quick", Desc: "fast alive/dead pass"},
			{Cmd: "scan runs"},
		},
	})
	cli.Register(cli.Command{
		Name:    "rank",
		Group:   cli.GroupScan,
		Summary: "recompute speed / clean / stability scores and ranks",
		Description: `Recomputes every proxy's speed, clean and stability score from the facts
in the database (no network) and ranks the alive proxies by --mode:
balanced (weights scan.weight_*), speed, clean or stable. 'scan full' does
this automatically.`,
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			fs.String(&scanOpts.rank, "m,mode", fs.Config.Scan.Rank, "rank order").Placeholder("MODE").
				Choices(score.RankModes...).ConfigKey("scan.rank")
			fs.Bool(&scanOpts.noTags, "no-tags", false, "do not refresh the heuristic tags too")
			fs.Int(&scanOpts.top, "top", 15, "show the N best proxies (0 = none)")
			fs.Bool(&scanOpts.json, "json", false, "print the result as JSON")
		},
		Examples: []cli.Example{
			{Cmd: "rank"},
			{Cmd: "rank -m speed --top 30"},
		},
		Run: runRank,
	})
	cli.Register(cli.Command{
		Name:    "lists",
		Group:   cli.GroupScan,
		Summary: "known-proxy / Tor / VPN / datacenter / abuse IP lists",
		Description: `Shows the IP lists scans check addresses against: lists of known open
proxies and anonymizers (which many sites block), Tor exits, VPN and
datacenter ranges, abuse feeds. Copies live in <home>/lists and are
refreshed when older than scan.lists_ttl. scan.custom_lists adds your own
(NAME|KIND|URL).`,
		Flags: func(fs *cli.FlagSet) {
			fs.Bool(&scanOpts.listsJSON, "json", false, "print the lists as JSON")
		},
		Run: runListsStatus,
		Subcommands: []cli.Command{
			{
				Name:      "update",
				Aliases:   []string{"fetch", "pull"},
				Summary:   "download stale lists (or --force all)",
				Usage:     "[options] [list...]",
				Args:      cli.ArgsDynamic("lists"),
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.Bool(&scanOpts.listsForce, "f,force", false, "download even fresh lists")
				},
				Run: runListsUpdate,
			},
			{
				Name:    "check",
				Aliases: []string{"lookup"},
				Summary: "show which lists (and DNS blacklists) an IP is on",
				Usage:   "[options] <ip|proxy>...",
				Flags: func(fs *cli.FlagSet) {
					fs.Bool(&scanOpts.checkDNSBL, "dnsbl", true, "also query the DNS blacklists")
					fs.Bool(&scanOpts.listsJSON, "json", false, "print the verdicts as JSON")
				},
				Examples: []cli.Example{
					{Cmd: "lists check 185.220.101.1"},
					{Cmd: "lists check socks5://1.2.3.4:1080 --no-dnsbl=true"},
				},
				Run: runListsCheck,
			},
		},
	})
	cli.RegisterCompleter("lists", func(ctx *cli.Context) []cli.Candidate {
		ls, _ := configuredLists(ctx.Config)
		var out []cli.Candidate
		for _, l := range ls {
			out = append(out, cli.Candidate{Value: l.Name, Desc: l.Desc})
		}
		return out
	})
}

// configuredLists returns the built-in plus custom lists, narrowed to
// scan.list_names when set.
func configuredLists(cfg *config.Config) ([]reputation.List, error) {
	all := append([]reputation.List(nil), reputation.Builtin...)
	for _, s := range cfg.Scan.CustomLists {
		l, err := reputation.ParseCustom(s)
		if err != nil {
			return nil, err
		}
		all = append(all, l)
	}
	return reputation.Select(all, cfg.Scan.ListNames)
}

func listManager(cfg *config.Config) (*reputation.Manager, error) {
	ls, err := configuredLists(cfg)
	if err != nil {
		return nil, err
	}
	return &reputation.Manager{Dir: paths.ListsDir(), TTL: cfg.Scan.ListsTTL.D(), Lists: ls}, nil
}

// scanRunOpts is what identifies a resumable run: same mode + same
// selection = same run.
type scanRunOpts struct {
	Mode string `json:"mode"`
	Key  string `json:"key"`
}

func runScan(ctx *cli.Context, mode string, args []string) int {
	o := &scanOpts
	f, err := o.filter.build(args)
	if err != nil {
		return ctx.UsageError("%v", err)
	}
	if o.unscanned {
		f.FullScanBefore = time.Unix(0, 1)
	}
	if o.olderThan > 0 {
		f.FullScanBefore = time.Now().Add(-o.olderThan.D())
	}
	var probes []probe.Probe
	for _, n := range strings.Split(o.probes, ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		p, err := probe.Get(n)
		if err != nil {
			return ctx.UsageError("%v", err)
		}
		probes = append(probes, p)
	}
	if len(probes) == 0 && (mode == scan.ModeFull || mode == scan.ModeQuick) {
		return ctx.UsageError("no probes (--probes / scan.probes)")
	}
	return withDB(ctx, func(d *db.DB) int { return doScan(ctx, d, mode, f, args, probes) })
}

func doScan(ctx *cli.Context, d *db.DB, mode string, f db.Filter, _ []string, probes []probe.Probe) int {
	o := &scanOpts
	ec := ctx.ErrColors()
	bg := context.WithoutCancel(ctx)
	j := scanJob{
		Mode: mode, Filter: f, Limit: o.filter.limit, Restart: o.restart,
		Workers: o.workers, Tries: o.tries, Timeout: o.timeout.D(), Delay: o.delay.D(), Probes: probes,
		NoDNSBL: o.noDNSBL, NoLists: o.noLists, NoGeo: o.noGeo, NoProto: o.noProto, NoJudge: o.noJudge,
		NoHTTPS: o.noHTTPS, MXToolbox: o.mxtoolbox, WIMIA: o.wimia, Offline: o.offline,
		Rank: o.rank, NoRank: o.noRank, NoTags: o.noTags,
		Logf:   ctx.Infof,
		Debugf: ctx.Debugf,
	}
	var progress *scanProgress
	j.OnStart = func(run *db.ScanRun, total, done int, extras string) {
		ctx.Infof("%s scan #%d: %d proxies, %d workers%s", mode, run.ID, total-done, o.workers, extras)
		progress = newScanProgress(ctx, mode, total, done)
		stopProgress = progress.start()
	}
	j.OnReport = func(r scan.Report, done, _ int) {
		if r.Alive && o.showAlive {
			progress.clear()
			printScanReport(ctx, r)
		}
		progress.update(done, r)
	}
	res, err := runScanJob(ctx, d, ctx.Config, j)
	if stopProgress != nil {
		stopProgress()
		stopProgress = nil
	}
	switch {
	case errors.Is(err, errNoMatch):
		ctx.Errorf("no proxies match")
		return 1
	case err != nil:
		ctx.Errorf("%v", err)
		return 1
	}
	summary := struct {
		scanResult
		Best []*db.Proxy `json:"best,omitempty"`
	}{scanResult: res}
	if o.top > 0 && res.Total > 0 {
		summary.Best, _ = d.ListProxies(bg, db.Filter{Status: []string{db.StatusAlive}, Sort: "rank", Limit: o.top,
			WithTags: true, IDs: aliveIDs(res.AliveIDs, mode)})
	}
	if o.json {
		return ctx.JSON(summary)
	}
	if !ctx.Quiet && res.Total > 0 {
		verdict := ec.Green + "done" + ec.Reset
		if res.Interrupted {
			verdict = ec.Yellow + "paused" + ec.Reset + fmt.Sprintf(" (run '%s scan %s' again to resume)", ctx.Prog, mode)
		}
		fmt.Fprintf(ctx.Stderr, "%s%s scan #%d%s %s: %d/%d in %s  %s%d alive%s  %s%d dead%s  %s%d listed%s  %d facts, %d verdicts\n",
			ec.Bold, mode, res.Run, ec.Reset, verdict, res.Done, res.Total, roundDur(res.ElapsedNS),
			ec.Green, res.Alive, ec.Reset, ec.Red, res.Dead, ec.Reset, ec.Yellow, res.Listed, ec.Reset,
			res.Writer.Measurements, res.Writer.Reputation)
		if res.Ranked > 0 {
			fmt.Fprintf(ctx.Stderr, "ranked %d alive proxies (%s); tags: %s\n", res.Ranked, o.rank, fmtTagCounts(res.Tags))
		}
	}
	if len(summary.Best) > 0 {
		c := ctx.Colors()
		fmt.Fprintf(ctx.Stdout, "\n%sBest proxies%s %s(rank: %s)%s\n", c.Bold, c.Reset, c.Gray, o.rank, c.Reset)
		printProxyTable(ctx, summary.Best)
	}
	if res.Interrupted {
		return 130
	}
	return 0
}

// stopProgress stops the running progress line (set by OnStart).
var stopProgress func()

// aliveIDs narrows the "best" table to what this scan found alive (all
// alive proxies for modes that do not test liveness).
func aliveIDs(ids []int64, mode string) []int64 {
	if mode != scan.ModeFull && mode != scan.ModeQuick {
		return nil
	}
	if len(ids) == 0 {
		return []int64{-1}
	}
	if len(ids) > 900 { // SQLite variable limit: the rank sort does the rest
		return nil
	}
	return ids
}

func scanExtras(o scan.Options) string {
	var parts []string
	if o.Mode == scan.ModeFull {
		parts = append(parts, fmt.Sprintf("%d probes x %d", len(o.Probes), o.Tries))
		if len(o.HTTPSURLs) > 0 {
			parts = append(parts, "https")
		}
		if o.JudgeURL != "" {
			parts = append(parts, "judge")
		}
		if o.Protocols {
			parts = append(parts, "protocols")
		}
	}
	if o.Lists != nil {
		parts = append(parts, fmt.Sprintf("%d lists", len(o.Lists)))
	}
	if o.DNSBL != nil {
		parts = append(parts, fmt.Sprintf("%d dnsbl", len(o.DNSBL.Zones)))
	}
	if o.Geo != nil {
		parts = append(parts, "geo")
	}
	if o.MXTB != nil {
		parts = append(parts, "mxtoolbox")
	}
	if o.WIMIA != nil {
		parts = append(parts, "wimia")
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func scoreOptions(cfg *config.Config, mode string) score.Options {
	return score.Options{Mode: mode, WeightSpeed: cfg.Scan.WeightSpeed, WeightClean: cfg.Scan.WeightClean,
		WeightStable: cfg.Scan.WeightStable, SpeedRefMS: cfg.Scan.SpeedRefMS}
}

func tagOptions(cfg *config.Config) score.TagOptions {
	t := cfg.Tags
	return score.TagOptions{StableN: t.StableN, FastN: t.FastN, FastMS: t.FastMS, FavMinUses: t.FavMinUses,
		FavMinRate: t.FavMinRate, RecommendTop: t.RecommendTop, FlakyMinFacts: t.FlakyMinFacts,
		FlakyMaxRate: t.FlakyMaxRate, Window: t.Window.D()}
}

func fmtTagCounts(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func printScanReport(ctx *cli.Context, r scan.Report) {
	c := ctx.Colors()
	var extra []string
	if r.Country != "" {
		extra = append(extra, r.Country)
	}
	if r.Anonymity != "" {
		extra = append(extra, r.Anonymity)
	}
	if r.MITM != nil && *r.MITM {
		extra = append(extra, c.Red+"TLS-MITM("+r.TLSIssuer+")"+c.Reset)
	} else if r.HTTPS != nil && *r.HTTPS {
		extra = append(extra, "https")
	}
	if len(r.Protocols) > 0 {
		extra = append(extra, strings.Join(r.Protocols, "/"))
	}
	listed := c.Green + "0 lists" + c.Reset
	if r.Listed > 0 {
		listed = fmt.Sprintf("%s%d lists%s (%s)", c.Red, r.Listed, c.Reset, strings.Join(r.ListedOn, ", "))
	}
	fmt.Fprintf(ctx.Stdout, "%s+%s %-28s %s%6s%s  %s  %s\n", c.Green, c.Reset, r.URL, msColor(c, r.MS), msOrDash(r.MS), c.Reset,
		strings.Join(extra, " "), listed)
}

// ---- progress line ------------------------------------------------------------

type scanProgress struct {
	ctx   *cli.Context
	mode  string
	total int
	base  int
	tty   bool
	mu    sync.Mutex
	done  int
	alive int
	last  string
	began time.Time
	shown bool
}

func newScanProgress(ctx *cli.Context, mode string, total, base int) *scanProgress {
	f, ok := ctx.Stderr.(*os.File)
	return &scanProgress{ctx: ctx, mode: mode, total: total, base: base, tty: ok && ui.IsTerminal(f) && !ctx.Quiet, done: base}
}

func (p *scanProgress) update(done int, r scan.Report) {
	p.mu.Lock()
	p.done = done
	if r.Alive {
		p.alive++
	}
	p.last = r.URL
	p.mu.Unlock()
}

func (p *scanProgress) clear() {
	if p.tty && p.shown {
		fmt.Fprint(p.ctx.Stderr, "\r\033[2K")
	}
}

func (p *scanProgress) render(w io.Writer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.ctx.ErrColors()
	elapsed := time.Since(p.began)
	rate := float64(p.done-p.base) / max(elapsed.Seconds(), 0.001)
	eta := "-"
	if rate > 0 {
		eta = roundDur(time.Duration(float64(p.total-p.done)/rate) * time.Second).String()
	}
	pct := 100 * float64(p.done) / float64(max(p.total, 1))
	width := 24
	filled := int(pct / 100 * float64(width))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	line := fmt.Sprintf("%s%s%s %s%s%s %d/%d %5.1f%%  %s%d alive%s  %.1f/s  ETA %s  %s%s%s",
		c.Cyan, p.mode, c.Reset, c.Magenta, bar, c.Reset, p.done, p.total, pct, c.Green, p.alive, c.Reset, rate, eta,
		c.Gray, p.last, c.Reset)
	if p.tty {
		fmt.Fprintf(w, "\r\033[2K%s", line)
		p.shown = true
	} else {
		fmt.Fprintln(w, stripANSI(line))
	}
}

// start begins rendering (every 250ms on a terminal, every 15s otherwise)
// and returns the stop function.
func (p *scanProgress) start() func() {
	p.began = time.Now()
	if p.ctx.Quiet {
		return func() {}
	}
	every := 15 * time.Second
	if p.tty {
		every = 250 * time.Millisecond
	}
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				p.render(p.ctx.Stderr)
			}
		}
	}()
	return func() {
		close(quit)
		<-finished
		p.clear()
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---- px scan runs / px rank / px lists ----------------------------------------------

func runScanRuns(ctx *cli.Context, _ []string) int {
	return withDB(ctx, func(d *db.DB) int {
		runs, err := d.ListScanRuns(ctx, scanOpts.runsLimit)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if scanOpts.json {
			if runs == nil {
				runs = []*db.ScanRun{}
			}
			return ctx.JSON(runs)
		}
		c := ctx.Colors()
		if len(runs) == 0 {
			fmt.Fprintf(ctx.Stdout, "%sno scans yet (px scan full)%s\n", c.Gray, c.Reset)
			return 0
		}
		fmt.Fprintf(ctx.Stdout, "%s%4s  %-10s  %-8s  %13s  %6s  %-19s  %s%s\n", c.Bold, "ID", "MODE", "STATE", "PROGRESS", "%", "STARTED", "TOOK", c.Reset)
		for _, r := range runs {
			sc := c.Gray
			switch r.State {
			case db.ScanDone:
				sc = c.Green
			case db.ScanRunning:
				sc = c.Cyan
			case db.ScanPaused:
				sc = c.Yellow
			case db.ScanFailed:
				sc = c.Red
			}
			took := "-"
			if !r.FinishedAt.IsZero() {
				took = roundDur(r.FinishedAt.Sub(r.StartedAt)).String()
			}
			pct := 100 * float64(r.Done) / float64(max(r.Total, 1))
			fmt.Fprintf(ctx.Stdout, "%4d  %-10s  %s%-8s%s  %13s  %5.1f%%  %-19s  %s\n", r.ID, r.Mode, sc, r.State, c.Reset,
				fmt.Sprintf("%d/%d", r.Done, r.Total), pct, r.StartedAt.Local().Format("2006-01-02 15:04:05"), took)
		}
		return 0
	})
}

func runRank(ctx *cli.Context, _ []string) int {
	return withDB(ctx, func(d *db.DB) int {
		res, err := score.Compute(ctx, d, scoreOptions(ctx.Config, scanOpts.rank))
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		var tr score.TagResult
		if !scanOpts.noTags {
			if tr, err = score.RefreshTags(ctx, d, tagOptions(ctx.Config)); err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
		}
		var best []*db.Proxy
		if scanOpts.top > 0 {
			best, _ = d.ListProxies(ctx, db.Filter{Status: []string{db.StatusAlive}, Sort: "rank", Limit: scanOpts.top, WithTags: true})
		}
		if scanOpts.json {
			return ctx.JSON(struct {
				Scores score.Result    `json:"scores"`
				Tags   score.TagResult `json:"tags"`
				Best   []*db.Proxy     `json:"best"`
			}{res, tr, best})
		}
		ctx.Infof("scored %d proxies, ranked %d alive (%s) in %s; tags: %s", res.Scored, res.Ranked, scanOpts.rank,
			roundDur(res.Elapsed+tr.Elapsed), fmtTagCounts(tr.Counts))
		if len(best) > 0 {
			printProxyTable(ctx, best)
		}
		return 0
	})
}

func runListsStatus(ctx *cli.Context, _ []string) int {
	m, err := listManager(ctx.Config)
	if err != nil {
		ctx.Errorf("%v", err)
		return 2
	}
	st := m.Status()
	if scanOpts.listsJSON {
		return ctx.JSON(st)
	}
	c := ctx.Colors()
	desc := map[string]string{}
	for _, l := range m.Lists {
		desc[l.Name] = l.Desc
	}
	fmt.Fprintf(ctx.Stdout, "%s%-20s %-11s %9s  %-8s  %s%s\n", c.Bold, "LIST", "KIND", "ENTRIES", "AGE", "DESCRIPTION", c.Reset)
	for _, s := range st {
		age, ac := "never", c.Yellow
		if !s.FetchedAt.IsZero() {
			age, ac = ago(s.FetchedAt), c.Gray
			if m.Fresh(reputation.List{Name: s.Name}) {
				ac = c.Green
			}
		}
		line := desc[s.Name]
		if s.Error != "" {
			line = c.Red + s.Error + c.Reset
		}
		fmt.Fprintf(ctx.Stdout, "%s%-20s%s %-11s %9d  %s%-8s%s  %s\n", c.Cyan, s.Name, c.Reset, s.Kind, s.Entries, ac, age, c.Reset, line)
	}
	fmt.Fprintf(ctx.Stdout, "\n%s%s; refreshed when older than %s (scan.lists_ttl); %d DNS blacklists in scan.dnsbl_zones%s\n",
		c.Gray, paths.Pretty(m.Dir), ctx.Config.Scan.ListsTTL, len(ctx.Config.Scan.DNSBLZones), c.Reset)
	return 0
}

func runListsUpdate(ctx *cli.Context, args []string) int {
	m, err := listManager(ctx.Config)
	if err != nil {
		ctx.Errorf("%v", err)
		return 2
	}
	if len(args) > 0 {
		if m.Lists, err = reputation.Select(m.Lists, args); err != nil {
			return ctx.UsageError("%v", err)
		}
	}
	c := ctx.Colors()
	failed := 0
	for _, r := range m.Load(ctx, true, scanOpts.listsForce) {
		if r.Err != nil && r.Set == nil {
			failed++
			fmt.Fprintf(ctx.Stdout, "%s✗%s %-20s %s\n", c.Red, c.Reset, r.Meta.Name, r.Err)
			continue
		}
		mark := c.Green + "✓" + c.Reset
		if r.Err != nil {
			mark = c.Yellow + "!" + c.Reset // kept the old copy
		}
		fmt.Fprintf(ctx.Stdout, "%s %-20s %8d entries  %s\n", mark, r.Meta.Name, r.Set.Entries, ago(r.Meta.FetchedAt))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func runListsCheck(ctx *cli.Context, args []string) int {
	if len(args) == 0 {
		return ctx.UsageError("give at least one IP or proxy")
	}
	m, err := listManager(ctx.Config)
	if err != nil {
		ctx.Errorf("%v", err)
		return 2
	}
	sets := reputation.Sets(m.Load(ctx, false, false))
	if len(sets) == 0 {
		ctx.Infof("no lists downloaded yet: run '%s lists update'", ctx.Prog)
	}
	var bl *reputation.DNSBL
	if scanOpts.checkDNSBL {
		bl = &reputation.DNSBL{Zones: ctx.Config.Scan.DNSBLZones, Timeout: ctx.Config.Scan.DNSTimeout.D()}
	}
	type verdict struct {
		IP     string                   `json:"ip"`
		Lists  []reputation.Match       `json:"lists"`
		DNSBL  []reputation.DNSBLResult `json:"dnsbl,omitempty"`
		Listed int                      `json:"listed"`
	}
	var out []verdict
	for _, a := range args {
		ip := scan.ExtractIP(a)
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			if addr, err = netip.ParseAddr(a); err != nil {
				ctx.Errorf("%q: no IP address", a)
				return 1
			}
		}
		v := verdict{IP: addr.String(), Lists: reputation.MatchAll(sets, addr)}
		if bl != nil {
			v.DNSBL = bl.Check(ctx, addr)
		}
		for _, l := range v.Lists {
			if l.Listed {
				v.Listed++
			}
		}
		for _, r := range v.DNSBL {
			if r.Listed {
				v.Listed++
			}
		}
		out = append(out, v)
	}
	if scanOpts.listsJSON {
		return ctx.JSON(out)
	}
	c := ctx.Colors()
	for _, v := range out {
		head := c.Green + "clean" + c.Reset
		if v.Listed > 0 {
			head = fmt.Sprintf("%slisted on %d%s", c.Red, v.Listed, c.Reset)
		}
		fmt.Fprintf(ctx.Stdout, "%s%s%s  %s\n", c.Bold, v.IP, c.Reset, head)
		for _, l := range v.Lists {
			if l.Listed {
				fmt.Fprintf(ctx.Stdout, "  %s✗%s %-20s %s\n", c.Red, c.Reset, l.List, l.Kind)
			} else if ctx.Verbose {
				fmt.Fprintf(ctx.Stdout, "  %s✓ %-20s %s%s\n", c.Gray, l.List, l.Kind, c.Reset)
			}
		}
		for _, r := range v.DNSBL {
			switch {
			case r.Err != nil:
				if ctx.Verbose {
					fmt.Fprintf(ctx.Stdout, "  %s? %-28s %v%s\n", c.Yellow, r.Zone, r.Err, c.Reset)
				}
			case r.Listed:
				fmt.Fprintf(ctx.Stdout, "  %s✗%s %-28s dnsbl %s\n", c.Red, c.Reset, r.Zone, strings.Join(r.Codes, ","))
			case ctx.Verbose:
				fmt.Fprintf(ctx.Stdout, "  %s✓ %-28s dnsbl%s\n", c.Gray, r.Zone, c.Reset)
			}
		}
	}
	return 0
}
