package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cache"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/router"
	"github.com/szmelc-inc/silkrouter/internal/source"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

func cmdRoute(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("route", flag.ContinueOnError)
	var (
		cachePath  = fs.String("f", cache.DefaultPath(), "proxy cache file")
		maxMS      = fs.Int("d", 1000, "max delay ms")
		minMS      = fs.Int("D", 0, "min delay ms")
		httpOnly   = fs.Bool("H", false, "http/https proxies only")
		socksOnly  = fs.Bool("s", false, "socks proxies only")
		force      = fs.String("p", "", "force a specific proxy (skips list & filters)")
		skipVerify = fs.Bool("x", false, "skip verification (fire through first pick)")
		doVerify   = fs.Bool("c", false, "verify candidates until one answers")
		vtimeout   = fs.Int("t", 8, "verify timeout (seconds)")
		fresh      = fs.Bool("r", false, "ignore cache, grab a fresh proxy from source")
		verbose    = fs.Bool("v", false, "print the exit IP before running")
		probeName  = fs.String("probe", probe.DefaultName, "probe used for verification")
		srcURL     = fs.String("u", "", "source URL for -r fresh mode")
		autoBuild  = fs.Bool("build", false, "auto-build the cache if missing (no prompt)")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: silkrouter route [options] [--] <command...>

Runs <command> with ALL_PROXY/HTTP_PROXY/HTTPS_PROXY set to a chosen proxy.

options:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fs.Usage()
		return 2
	}

	pr, err := probe.Get(*probeName)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
		return 2
	}

	verify := router.VerifyAuto
	if *skipVerify {
		verify = router.VerifySkip
	}
	if *doVerify {
		verify = router.VerifyDo
	}

	origin := source.Origin{URL: *srcURL}

	// First-run handling: if the cache is missing (and we're not forcing a
	// proxy or already in fresh mode), offer to build one.
	if *force == "" && !*fresh {
		if _, statErr := os.Stat(*cachePath); errors.Is(statErr, os.ErrNotExist) {
			if *autoBuild || askYesNo(fmt.Sprintf("silkrouter: no cache at %s. Build a tested, sorted cache?", *cachePath)) {
				ps, err := origin.Candidates(ctx, 30*time.Second)
				if err != nil {
					ui.Errf("silkrouter: %v\n", err)
					return 1
				}
				cfg := buildConfig{
					cachePath: *cachePath, probe: pr, jobs: 200, tries: 2,
					timeout: 15 * time.Second, srcDesc: origin.Describe(),
				}
				if code, err := doBuild(ctx, ps, cfg); err != nil || code != 0 {
					ui.Errf("silkrouter: build failed, falling back to fresh proxies\n")
					*fresh = true
				}
			} else {
				*fresh = true
			}
		}
	}

	opts := router.Options{
		CachePath:     *cachePath,
		Force:         *force,
		MinMS:         *minMS,
		MaxMS:         *maxMS,
		HTTPOnly:      *httpOnly,
		SocksOnly:     *socksOnly,
		Verify:        verify,
		VerifyTimeout: time.Duration(*vtimeout) * time.Second,
		Fresh:         *fresh,
		Verbose:       *verbose,
		Probe:         pr,
		Origin:        origin,
	}

	code, err := router.Run(ctx, opts, cmdArgs)
	if err != nil {
		if errors.Is(err, router.ErrNoCache) {
			ui.Errf("silkrouter: no cache at %s (build one, or use -r for fresh)\n", *cachePath)
			return 1
		}
		ui.Errf("silkrouter: %v\n", err)
	}
	return code
}

// askYesNo prompts on stderr and reads a Y/n answer from stdin. Defaults to yes
// on empty input; returns false only for an explicit "n".
func askYesNo(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s [Y/n] ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return true // non-interactive: assume yes
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return !strings.HasPrefix(ans, "n")
}
