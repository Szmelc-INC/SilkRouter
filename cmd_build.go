package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cache"
	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/source"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

// buildConfig captures everything doBuild needs, independent of flag parsing so
// the route subcommand can reuse it for its first-run auto-build.
type buildConfig struct {
	cachePath string
	probe     probe.Probe
	jobs      int
	tries     int
	timeout   time.Duration
	showAll   bool
	overwrite bool
	srcDesc   string
	// writeCache writes the text cache at cachePath (legacy mode, or
	// cache.mirror); rec records every result into the database (nil =
	// off).
	writeCache bool
	rec        *dbRecorder
}

var buildOpts struct {
	cache     string
	srcURL    string
	srcFile   string
	builtin   bool
	probe     string
	testURL   string
	jobs      int
	tries     int
	wait      config.Duration
	showAll   bool
	overwrite bool
}

func init() {
	cli.Register(cli.Command{
		Name:    "build",
		Group:   cli.GroupCache,
		Summary: "build/append the proxy cache (test & sort proxies)",
		Description: `Downloads the candidate list (or reads -F FILE, or the list embedded in
the binary with -builtin), tests every proxy with the chosen probe and
records every result (alive or dead) in the database, which route then
picks from.

With a legacy text cache selected (-f FILE, $PX_LIST or cache.file) the
working proxies are also merged into that file, sorted fastest first:
existing entries are kept (new timings win); -O overwrites it instead. A
run that finds no working proxy never replaces an existing cache.
cache.mirror = true keeps <home>/cache.list in sync in database mode too.`,
		NeedsHome: true,
		Flags:     func(fs *cli.FlagSet) { buildFlags(fs, false) },
		Examples: []cli.Example{
			{Cmd: "build", Desc: "test the upstream list with the default probe"},
			{Cmd: "build -probe google -j 400 -n 1", Desc: "faster, rougher pass"},
			{Cmd: "build -builtin -O", Desc: "offline: rebuild from the embedded list"},
			{Cmd: "build -F my-proxies.txt", Desc: "test a local list"},
		},
		Run: func(ctx *cli.Context, args []string) int { return runBuild(ctx, args, false) },
	})
	cli.Register(cli.Command{
		Name:    "recache",
		Group:   cli.GroupCache,
		Summary: "re-test the proxies already in the cache (prune dead)",
		Description: `Re-measures every alive proxy in the database and records the results
(the ones that stopped answering become dead). With a legacy text cache
selected (-f FILE, $PX_LIST or cache.file) it re-tests that file instead and
rewrites it with the proxies that still answer, sorted fastest first.`,
		NeedsHome: true,
		Flags:     func(fs *cli.FlagSet) { buildFlags(fs, true) },
		Examples: []cli.Example{
			{Cmd: "recache -j 100 -w 10"},
		},
		Run: func(ctx *cli.Context, args []string) int { return runBuild(ctx, args, true) },
	})
	cli.Register(cli.Command{
		Name:    "list",
		Aliases: []string{"ls"},
		Group:   cli.GroupCache,
		Summary: "print a report of the working proxies (database or text cache)",
		Description: `Prints how many working proxies there are by protocol and latency
bucket: the alive proxies of the database, or the legacy text cache when
one is selected (-f FILE, $PX_LIST or cache.file). See 'proxies' for a
full table and 'db info' for the whole database.`,
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			fs.String(&listOpts.cache, "f,cache", fs.Config.CacheFile(), "proxy cache file").
				Placeholder("FILE").Files().ConfigKey("cache.file")
			fs.Bool(&listOpts.json, "json", false, "print the cache as JSON")
		},
		Examples: []cli.Example{
			{Cmd: "list"},
			{Cmd: "ls --json | jq '.proxies[0]'"},
		},
		Run: runList,
	})
}

func buildFlags(fs *cli.FlagSet, recache bool) {
	cfg := fs.Config
	o := &buildOpts
	fs.String(&o.cache, "f,cache", cfg.CacheFile(), "proxy cache file").
		Placeholder("FILE").Files().ConfigKey("cache.file")
	// Source flags are meaningless for recache but stay accepted there.
	src := []*cli.Flag{
		fs.String(&o.srcURL, "u,url", cfg.Source.URL, "source proxy-list URL (default: upstream list)").
			Placeholder("URL").ConfigKey("source.url"),
		fs.String(&o.srcFile, "F,from-file", "", "build from a local proxy-list file").
			Placeholder("FILE").Files(),
		fs.Bool(&o.builtin, "builtin", cfg.Source.Builtin, "use the proxy list embedded in the binary").
			ConfigKey("source.builtin"),
	}
	fs.String(&o.probe, "probe", cfg.Probe.Name, "caching probe (see: probes)").
		Placeholder("NAME").Dynamic("probes").ConfigKey("probe.name")
	fs.String(&o.testURL, "test-url", cfg.Probe.TestURL, "custom probe URL (any 2xx counts as alive)").
		Placeholder("URL").ConfigKey("probe.test_url")
	fs.Int(&o.jobs, "j,workers,jobs", cfg.Build.Workers, "parallel workers").ConfigKey("build.workers")
	fs.Int(&o.tries, "n,tries", cfg.Build.Tries, "samples averaged per proxy").ConfigKey("build.tries")
	fs.Duration(&o.wait, "w,timeout", cfg.Build.Timeout, "per-try timeout").ConfigKey("build.timeout")
	fs.Bool(&o.showAll, "a,show-dead", cfg.Build.ShowDead, "also log dead proxies").ConfigKey("build.show_dead")
	ow := fs.Bool(&o.overwrite, "O,overwrite", false, "overwrite the cache instead of append-merging")
	if recache {
		for _, f := range append(src, ow) {
			f.Hide()
		}
	}
}

// runBuild gathers candidates and runs the build (or recache).
func runBuild(ctx *cli.Context, _ []string, recache bool) int {
	o := &buildOpts
	pr, err := selectProbe(o.probe, o.testURL)
	if err != nil {
		ctx.Errorf("%v", err)
		return 2
	}

	textMode := textCacheMode(ctx)
	cfg := buildConfig{
		cachePath:  o.cache,
		probe:      pr,
		jobs:       o.jobs,
		tries:      o.tries,
		timeout:    o.wait.D(),
		showAll:    o.showAll,
		overwrite:  o.overwrite || recache, // recache always prunes -> overwrite
		writeCache: textMode || ctx.Config.Cache.Mirror,
	}
	if !textMode || ctx.Config.DB.Record {
		cfg.rec = newRecorder(ctx)
		defer cfg.rec.close()
	}
	if cfg.rec == nil && !textMode {
		cfg.writeCache = true // database unavailable: keep the text cache going
	}

	var candidates []proxy.Proxy
	switch {
	case recache && textMode:
		existing, err := cache.Load(o.cache)
		if err != nil {
			ctx.Errorf("no cache to re-test at %s: %v", o.cache, err)
			return 1
		}
		for i := range existing {
			existing[i].LatencyMS = 0 // force a fresh measurement
		}
		candidates = existing
		cfg.srcDesc = "re-cache " + o.cache
	case recache:
		if cfg.rec == nil {
			ctx.Errorf("no cache to re-test: the database is not available")
			return 1
		}
		alive, err := cfg.rec.d.ListProxies(ctx, db.Filter{Status: []string{db.StatusAlive}, Sort: "last_ms"})
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if len(alive) == 0 {
			ctx.Errorf("no cache to re-test: %v", errNoAlive)
			return 1
		}
		for _, p := range alive {
			pp := p.ToProxy()
			pp.LatencyMS = 0
			candidates = append(candidates, pp)
		}
		cfg.srcDesc = "re-cache of the database"
	default:
		origin := source.Origin{URL: o.srcURL, File: o.srcFile, UseBuiltin: o.builtin}
		cfg.srcDesc = origin.Describe()
		ps, err := origin.Candidates(ctx, ctx.Config.Source.Timeout.D())
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		candidates = ps
		if cfg.rec != nil {
			if _, err := cfg.rec.d.ImportProxies(ctx, ps, db.ImportOptions{Source: originLabel(origin)}); err != nil {
				ctx.Errorf("warning: database: %v", err)
			}
		}
	}

	code, err := doBuild(ctx, candidates, cfg)
	if err != nil {
		ctx.Errorf("%v", err)
	}
	return code
}

// originLabel is the database source label of a candidate origin.
func originLabel(o source.Origin) string {
	switch {
	case o.UseBuiltin:
		return db.SourceBuiltin
	case o.File != "":
		return "file:" + filepath.Base(o.File)
	case o.URL != "":
		return "url:" + urlHost(o.URL)
	default:
		return "url:" + urlHost(source.DefaultURL)
	}
}

// doBuild tests candidates and writes them into the cache (merge or overwrite).
func doBuild(ctx *cli.Context, candidates []proxy.Proxy, cfg buildConfig) (int, error) {
	if len(candidates) == 0 {
		return 1, fmt.Errorf("no proxies from %s", cfg.srcDesc)
	}
	ctx.Infof("testing %d proxies from %s (probe=%s jobs=%d tries=%d, %s/try) ...",
		len(candidates), cfg.srcDesc, cfg.probe.Name, cfg.jobs, cfg.tries, cfg.timeout)

	bo := cache.BuildOptions{
		Probe:    cfg.probe,
		Jobs:     cfg.jobs,
		Tries:    cfg.tries,
		Timeout:  cfg.timeout,
		ShowAll:  cfg.showAll,
		Progress: !ctx.Quiet,
	}
	if cfg.rec != nil {
		w, probeName := cfg.rec.w, cfg.probe.Name
		bo.OnResult = func(p proxy.Proxy, r probe.Result) {
			_ = w.Measure(db.Measurement{
				Addr: db.AddrOf(p), Kind: db.KindProbe, Probe: probeName, OK: r.OK,
				MS: r.LatencyMS, ErrClass: probe.ClassifyErr(r.Err), ExitIP: r.ExitIP,
			})
		}
	}
	res := cache.Build(ctx, candidates, bo, os.Stderr)

	c := ui.Colors()
	if cfg.rec != nil {
		if err := cfg.rec.w.Flush(); err != nil {
			ctx.Errorf("warning: database: %v", err)
		}
		st := cfg.rec.w.Stats()
		ctx.Infof("recorded %d results (%s%d working%s) into %s", st.Measurements,
			c.Green, len(res.Working), c.Reset, paths.Pretty(cfg.rec.d.Path()))
	}

	if len(res.Working) == 0 {
		if cfg.overwrite && cfg.writeCache {
			if _, err := os.Stat(cfg.cachePath); err == nil {
				ctx.Errorf("keeping existing cache (refusing to overwrite with 0 results)")
			}
		}
		return 1, fmt.Errorf("0 working proxies this run")
	}
	if !cfg.writeCache {
		return 0, nil
	}

	final := res.Working
	if !cfg.overwrite {
		existing, _ := cache.Load(cfg.cachePath) // missing cache => nil, fine
		final = cache.Merge(existing, res.Working)
	}
	if err := cache.Save(cfg.cachePath, final); err != nil {
		return 1, err
	}

	if cfg.overwrite {
		ctx.Infof("wrote %s%d working%s -> %s (overwrite)",
			c.Green, len(res.Working), c.Reset, cfg.cachePath)
	} else {
		ctx.Infof("merged %s%d new%s into %s -> %d total (deduped)",
			c.Green, len(res.Working), c.Reset, cfg.cachePath, len(final))
	}
	return 0, nil
}

var listOpts struct {
	cache string
	json  bool
}

type listEntry struct {
	URL       string `json:"url"`
	Scheme    string `json:"scheme"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	LatencyMS int    `json:"latency_ms"`
}

// runList prints a report of the working proxies: the database's alive
// proxies, or the legacy text cache when one is selected.
func runList(ctx *cli.Context, _ []string) int {
	var ps []proxy.Proxy
	label := listOpts.cache
	if textCacheMode(ctx) {
		var err error
		if ps, err = cache.Load(listOpts.cache); err != nil {
			ctx.Errorf("no cache at %s", listOpts.cache)
			return 1
		}
	} else {
		code := withDB(ctx, func(d *db.DB) int {
			alive, err := d.ListProxies(ctx, db.Filter{Status: []string{db.StatusAlive}, Sort: "last_ms"})
			if err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
			for _, p := range alive {
				ps = append(ps, p.ToProxy())
			}
			label = "database " + d.Path()
			return 0
		})
		if code != 0 {
			return code
		}
	}
	if listOpts.json {
		out := struct {
			Cache   string      `json:"cache"`
			Count   int         `json:"count"`
			Proxies []listEntry `json:"proxies"`
		}{Cache: label, Count: len(ps), Proxies: []listEntry{}}
		for _, p := range ps {
			out.Proxies = append(out.Proxies, listEntry{
				URL: p.URL(), Scheme: string(p.Scheme), Host: p.Host, Port: p.Port, LatencyMS: p.LatencyMS,
			})
		}
		return ctx.JSON(out)
	}
	cache.Report(ctx.Stdout, label, ps)
	return 0
}

// selectProbe resolves a probe by name, or builds a custom one from testURL.
func selectProbe(name, testURL string) (probe.Probe, error) {
	if testURL != "" {
		return probe.Custom(testURL), nil
	}
	return probe.Get(name)
}
