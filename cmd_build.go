package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cache"
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
}

// cmdBuild parses build/recache flags, gathers candidates and runs the build.
func cmdBuild(ctx context.Context, args []string, recache bool) int {
	name := "build"
	if recache {
		name = "recache"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var (
		cachePath = fs.String("f", cache.DefaultPath(), "proxy cache file")
		srcURL    = fs.String("u", "", "source proxy-list URL (default: upstream list)")
		srcFile   = fs.String("F", "", "build from a local proxy-list file")
		builtin   = fs.Bool("builtin", false, "use the proxy list embedded in the binary")
		probeName = fs.String("probe", probe.DefaultName, "caching probe (see: silkrouter probes)")
		testURL   = fs.String("test-url", "", "custom probe URL (any 2xx counts as alive)")
		jobs      = fs.Int("j", 200, "parallel workers")
		tries     = fs.Int("n", 2, "samples averaged per proxy")
		wait      = fs.Int("w", 15, "per-try timeout (seconds)")
		showAll   = fs.Bool("a", false, "also log DEAD proxies")
		overwrite = fs.Bool("O", false, "overwrite the cache instead of append-merging")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: silkrouter %s [options]\n\n", name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	pr, err := selectProbe(*probeName, *testURL)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
		return 2
	}

	cfg := buildConfig{
		cachePath: *cachePath,
		probe:     pr,
		jobs:      *jobs,
		tries:     *tries,
		timeout:   time.Duration(*wait) * time.Second,
		showAll:   *showAll,
		overwrite: *overwrite || recache, // recache always prunes -> overwrite
	}

	// Gather candidates from the chosen origin.
	var candidates []proxy.Proxy
	if recache {
		existing, err := cache.Load(*cachePath)
		if err != nil {
			ui.Errf("silkrouter: no cache to re-test at %s: %v\n", *cachePath, err)
			return 1
		}
		for i := range existing {
			existing[i].LatencyMS = 0 // force a fresh measurement
		}
		candidates = existing
		cfg.srcDesc = "re-cache " + *cachePath
	} else {
		origin := source.Origin{URL: *srcURL, File: *srcFile, UseBuiltin: *builtin}
		cfg.srcDesc = origin.Describe()
		ps, err := origin.Candidates(ctx, 30*time.Second)
		if err != nil {
			ui.Errf("silkrouter: %v\n", err)
			return 1
		}
		candidates = ps
	}

	code, err := doBuild(ctx, candidates, cfg)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
	}
	return code
}

// doBuild tests candidates and writes them into the cache (merge or overwrite).
func doBuild(ctx context.Context, candidates []proxy.Proxy, cfg buildConfig) (int, error) {
	if len(candidates) == 0 {
		return 1, fmt.Errorf("no proxies from %s", cfg.srcDesc)
	}
	ui.Errf("silkrouter: testing %d proxies from %s (probe=%s jobs=%d tries=%d, %s/try) ...\n",
		len(candidates), cfg.srcDesc, cfg.probe.Name, cfg.jobs, cfg.tries, cfg.timeout)

	res := cache.Build(ctx, candidates, cache.BuildOptions{
		Probe:    cfg.probe,
		Jobs:     cfg.jobs,
		Tries:    cfg.tries,
		Timeout:  cfg.timeout,
		ShowAll:  cfg.showAll,
		Progress: true,
	}, os.Stderr)

	if len(res.Working) == 0 {
		if cfg.overwrite {
			if _, err := os.Stat(cfg.cachePath); err == nil {
				ui.Errf("silkrouter: keeping existing cache (refusing to overwrite with 0 results)\n")
			}
		}
		return 1, fmt.Errorf("0 working proxies this run")
	}

	final := res.Working
	if !cfg.overwrite {
		existing, _ := cache.Load(cfg.cachePath) // missing cache => nil, fine
		final = cache.Merge(existing, res.Working)
	}
	if err := cache.Save(cfg.cachePath, final); err != nil {
		return 1, err
	}

	c := ui.Colors()
	if cfg.overwrite {
		ui.Errf("silkrouter: wrote %s%d working%s -> %s (overwrite)\n",
			c.Green, len(res.Working), c.Reset, cfg.cachePath)
	} else {
		ui.Errf("silkrouter: merged %s%d new%s into %s -> %d total (deduped)\n",
			c.Green, len(res.Working), c.Reset, cfg.cachePath, len(final))
	}
	return 0, nil
}

// cmdList prints a report of the current cache.
func cmdList(_ context.Context, args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	cachePath := fs.String("f", cache.DefaultPath(), "proxy cache file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ps, err := cache.Load(*cachePath)
	if err != nil {
		ui.Errf("silkrouter: no cache at %s\n", *cachePath)
		return 1
	}
	cache.Report(os.Stdout, *cachePath, ps)
	return 0
}

// selectProbe resolves a probe by name, or builds a custom one from testURL.
func selectProbe(name, testURL string) (probe.Probe, error) {
	if testURL != "" {
		return probe.Custom(testURL), nil
	}
	return probe.Get(name)
}
