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

// cmdCheck tests a single proxy or a whole file of proxies for liveness and
// latency using a caching probe.
func cmdCheck(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	var (
		probeName = fs.String("probe", probe.DefaultName, "caching probe to use")
		testURL   = fs.String("test-url", "", "custom probe URL (any 2xx = alive)")
		timeoutS  = fs.Int("t", 8, "per-try timeout (seconds)")
		tries     = fs.Int("n", 1, "samples averaged per proxy")
		jobs      = fs.Int("j", 100, "parallel workers (file mode)")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: silkrouter check [options] <proxy | file>\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	target := fs.Arg(0)

	pr, err := selectProbe(*probeName, *testURL)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
		return 2
	}
	timeout := time.Duration(*timeoutS) * time.Second
	c := ui.Colors()

	// File mode when the argument is an existing readable file.
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		data, err := os.ReadFile(target)
		if err != nil {
			ui.Errf("silkrouter: %v\n", err)
			return 1
		}
		proxies := source.ParseLines(data)
		if len(proxies) == 0 {
			ui.Errf("silkrouter: no valid proxies in %s\n", target)
			return 1
		}
		res := cache.Build(ctx, proxies, cache.BuildOptions{
			Probe: pr, Jobs: *jobs, Tries: *tries, Timeout: timeout, Progress: true,
		}, os.Stderr)
		fmt.Printf("%d/%d proxies alive\n", len(res.Working), res.Tested)
		for _, p := range res.Working {
			fmt.Printf("%s%-40s%s %dms\n", c.Green, p.URL(), c.Reset, p.LatencyMS)
		}
		if len(res.Working) == 0 {
			return 1
		}
		return 0
	}

	// Single-proxy mode.
	p, err := proxy.Parse(target)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
		return 2
	}
	r := pr.Measure(ctx, p, *tries, timeout)
	if !r.OK {
		fmt.Printf("%s%s%s  %sDEAD%s  (%v)\n", c.Bold, p.URL(), c.Reset, c.Red, c.Reset, r.Err)
		return 1
	}
	line := fmt.Sprintf("%s%s%s  %sALIVE%s  %dms", c.Bold, p.URL(), c.Reset, c.Green, c.Reset, r.LatencyMS)
	if r.ExitIP != "" {
		line += fmt.Sprintf("  exit=%s", r.ExitIP)
	}
	fmt.Println(line)
	return 0
}

// cmdProbes lists the available caching probes.
func cmdProbes(_ context.Context, _ []string) int {
	c := ui.Colors()
	fmt.Println("Available caching probes (silkrouter build -probe <name>):")
	for _, p := range probe.All() {
		marker := "  "
		if p.Name == probe.DefaultName {
			marker = c.Green + "* " + c.Reset
		}
		fmt.Printf("%s%-12s %s\n", marker, p.Name, p.Desc)
	}
	fmt.Println("\nOr use -test-url <URL> for a custom probe (any 2xx response = alive).")
	return 0
}
