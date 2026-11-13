package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/router"
	"github.com/szmelc-inc/silkrouter/internal/source"
)

var routeOpts struct {
	cache      string
	maxMS      int
	minMS      int
	httpOnly   bool
	socksOnly  bool
	force      string
	skipVerify bool
	doVerify   bool
	vtimeout   config.Duration
	fresh      bool
	verbose    bool
	probe      string
	testURL    string
	srcURL     string
	autoBuild  bool
}

func init() {
	cli.Register(cli.Command{
		Name:    "route",
		Aliases: []string{"run"},
		Group:   cli.GroupRoute,
		Summary: "run a command through a cached/random proxy",
		Description: `Runs <command> with ALL_PROXY, HTTP_PROXY and HTTPS_PROXY (upper and
lower case) set to a proxy picked at random from the alive proxies of the
database, filtered by latency and protocol. With -f FILE, $PX_LIST,
cache.file or route.pool = cache it picks from that legacy text cache
instead. Verifications (-c) are recorded in the database as preflight
measurements.

This is also what happens when the first argument is not a known command:
"px curl ifconfig.me" is "px route curl ifconfig.me", and px.sh-style
options work in front of it ("px -d 500 -s curl ..."). Use "px -- <command>"
to route a program whose name matches a subcommand.

When there is nothing tested to pick from, route offers to build; decline and it grabs
an untested proxy straight from the source (verified first).`,
		Usage:       "[options] [--] <command> [args...]",
		Args:        cli.ArgsCommand,
		PassThrough: true,
		NeedsHome:   true,
		Flags:       routeFlags,
		Examples: []cli.Example{
			{Cmd: "curl -s https://ifconfig.me", Desc: "route a command (legacy px.sh style)"},
			{Cmd: "route -- curl -s https://ifconfig.me"},
			{Cmd: "-s -d 500 git pull", Desc: "socks proxies under 500 ms only"},
			{Cmd: "route -c -v -- wget https://example.com", Desc: "verify the pick first and print its exit IP"},
			{Cmd: "-- build.sh", Desc: "route a program that shares a subcommand's name"},
		},
		Run: runRoute,
	})
}

func routeFlags(fs *cli.FlagSet) {
	cfg := fs.Config
	o := &routeOpts
	fs.String(&o.cache, "f,cache", cfg.CacheFile(), "proxy cache file").
		Placeholder("FILE").Files().ConfigKey("cache.file")
	fs.Int(&o.maxMS, "d,max-ms", cfg.Route.MaxMS, "maximum cached latency (0 = no limit)").
		Placeholder("MS").ConfigKey("route.max_ms")
	fs.Int(&o.minMS, "D,min-ms", cfg.Route.MinMS, "minimum cached latency").
		Placeholder("MS").ConfigKey("route.min_ms")
	fs.Bool(&o.httpOnly, "H,http", cfg.Route.Scheme == "http", "http/https proxies only").ConfigKey("route.scheme")
	fs.Bool(&o.socksOnly, "s,socks", cfg.Route.Scheme == "socks", "socks proxies only").ConfigKey("route.scheme")
	fs.String(&o.force, "p,proxy", "", "use this proxy (skips the cache and all filters)").Placeholder("URL")
	fs.Bool(&o.skipVerify, "x,no-verify", cfg.Route.Verify == "skip", "skip verification, fire through the first pick").
		ConfigKey("route.verify")
	fs.Bool(&o.doVerify, "c,verify", cfg.Route.Verify == "always", "verify candidates until one answers").
		ConfigKey("route.verify")
	fs.Duration(&o.vtimeout, "t,timeout", cfg.Route.VerifyTimeout, "timeout of one verification").
		ConfigKey("route.verify_timeout")
	fs.Bool(&o.fresh, "r,fresh", false, "ignore the cache, grab a fresh proxy from the source")
	fs.Bool(&o.verbose, "v,verbose", false, "print the exit IP before running (also: global -v)")
	fs.String(&o.probe, "probe", cfg.Probe.Name, "probe used for verification").
		Placeholder("NAME").Dynamic("probes").ConfigKey("probe.name")
	fs.String(&o.testURL, "test-url", cfg.Probe.TestURL, "custom probe URL (any 2xx = alive)").
		Placeholder("URL").ConfigKey("probe.test_url")
	fs.String(&o.srcURL, "u,url", cfg.Source.URL, "source list URL for fresh mode (-r)").
		Placeholder("URL").ConfigKey("source.url")
	fs.Bool(&o.autoBuild, "build", cfg.Route.AutoBuild, "build the cache without asking when it is missing").
		ConfigKey("route.auto_build")
}

func runRoute(ctx *cli.Context, args []string) int {
	o := &routeOpts
	cfg := ctx.Config
	if len(args) == 0 {
		return ctx.UsageError("missing the command to run")
	}
	if _, err := exec.LookPath(args[0]); err != nil {
		if ctx.Legacy && !strings.ContainsAny(args[0], `/\`) {
			msg := fmt.Sprintf("%q is neither a %s command nor a program in $PATH", args[0], ctx.Prog)
			if s := ctx.App.Registry.Suggest(args[0]); len(s) > 0 {
				msg += fmt.Sprintf(" (did you mean '%s %s'?)", ctx.Prog, s[0])
			}
			ctx.Errorf("%s", msg)
			fmt.Fprintf(ctx.Stderr, "Run '%s help' for usage.\n", ctx.Prog)
		} else {
			ctx.Errorf("%s: command not found", args[0])
		}
		return 127
	}

	pr, err := selectProbe(o.probe, o.testURL)
	if err != nil {
		ctx.Errorf("%v", err)
		return 2
	}

	// Explicit flags beat config-derived defaults of their counterpart.
	httpOnly, socksOnly := o.httpOnly, o.socksOnly
	if ctx.Flags.Changed("s") && !ctx.Flags.Changed("H") {
		httpOnly = false
	}
	if ctx.Flags.Changed("H") && !ctx.Flags.Changed("s") {
		socksOnly = false
	}
	skip, do := o.skipVerify, o.doVerify
	if ctx.Flags.Changed("x") && !ctx.Flags.Changed("c") {
		do = false
	}
	if ctx.Flags.Changed("c") && !ctx.Flags.Changed("x") {
		skip = false
	}
	verify := router.VerifyAuto
	if skip {
		verify = router.VerifySkip
	}
	if do {
		verify = router.VerifyDo
	}

	origin := source.Origin{URL: o.srcURL, UseBuiltin: cfg.Source.Builtin}
	fresh := o.fresh

	// Pool: the database by default; the legacy text cache with -f,
	// $PX_LIST, cache.file or route.pool = cache. -p and -r need neither.
	var d *db.DB
	if o.force == "" && !fresh && !textCacheMode(ctx) && cfg.Route.Pool != "cache" {
		var err error
		if d, err = openDB(ctx); err != nil {
			ctx.Errorf("warning: database unavailable (%v); using the text cache %s", err, o.cache)
			d = nil
		} else {
			defer d.Close()
		}
	}
	filter := db.Filter{
		Status: []string{db.StatusAlive}, MinMS: o.minMS, MaxMS: o.maxMS, IncludeUntimed: true,
	}
	switch {
	case httpOnly:
		filter.Schemes = db.ExpandSchemes([]string{"http"}, true)
	case socksOnly:
		filter.Schemes = db.ExpandSchemes([]string{"socks"}, false)
	}

	// First-run handling: if there is nothing tested to pick from (and
	// we're not forcing a proxy or already in fresh mode), offer to build.
	if o.force == "" && !fresh {
		missing, what := false, "no cache at "+o.cache
		if d != nil {
			n, err := d.CountProxies(ctx, db.Filter{Status: []string{db.StatusAlive}})
			missing, what = err == nil && n == 0, "no tested proxies in the database"
		} else if _, statErr := os.Stat(o.cache); errors.Is(statErr, os.ErrNotExist) {
			missing = true
		}
		if missing {
			prompt := fmt.Sprintf("%s: %s. Build a tested, sorted cache?", ctx.Prog, what)
			if o.autoBuild || askYesNo(ctx.Stdin, ctx.Stderr, prompt) {
				ps, err := origin.Candidates(ctx, cfg.Source.Timeout.D())
				if err != nil {
					ctx.Errorf("%v", err)
					return 1
				}
				bc := buildConfig{
					cachePath: o.cache, probe: pr, jobs: cfg.Build.Workers, tries: cfg.Build.Tries,
					timeout: cfg.Build.Timeout.D(), srcDesc: origin.Describe(),
					writeCache: d == nil || cfg.Cache.Mirror,
				}
				if d != nil {
					if _, err := d.ImportProxies(ctx, ps, db.ImportOptions{Source: originLabel(origin)}); err != nil {
						ctx.Errorf("warning: database: %v", err)
					}
					bc.rec = &dbRecorder{d: d, w: d.NewWriter(ctx, db.WriterOptions{
						BatchSize: cfg.DB.BatchSize, FlushInterval: cfg.DB.FlushInterval.D(),
						OnError: func(err error, n int) {
							ctx.Errorf("warning: database: %d rows not recorded: %v", n, err)
						},
					})}
				}
				code, err := doBuild(ctx, ps, bc)
				if bc.rec != nil {
					bc.rec.w.Close() // the database itself stays open
				}
				if err != nil || code != 0 {
					ctx.Errorf("build failed, falling back to fresh proxies")
					fresh = true
				}
			} else {
				fresh = true
			}
		}
	}

	opts := router.Options{
		CachePath:     o.cache,
		Force:         o.force,
		MinMS:         o.minMS,
		MaxMS:         o.maxMS,
		HTTPOnly:      httpOnly,
		SocksOnly:     socksOnly,
		Verify:        verify,
		VerifyTimeout: o.vtimeout.D(),
		Fresh:         fresh,
		Verbose:       o.verbose || ctx.Verbose,
		Quiet:         ctx.Quiet,
		Probe:         pr,
		Origin:        origin,
		SourceTimeout: cfg.Source.Timeout.D(),
	}
	if d != nil && !fresh {
		rows, err := d.ListProxies(ctx, filter)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		opts.Pool = make([]proxy.Proxy, 0, len(rows))
		for _, p := range rows {
			opts.Pool = append(opts.Pool, p.ToProxy())
		}
		opts.PoolLabel = "database"
	}
	if d != nil && cfg.DB.Record {
		opts.OnVerify = func(p proxy.Proxy, r probe.Result) {
			if _, err := d.RecordMeasurement(ctx, db.Measurement{
				Addr: db.AddrOf(p), Kind: db.KindPreflight, Probe: pr.Name, OK: r.OK,
				MS: r.LatencyMS, ErrClass: probe.ClassifyErr(r.Err), ExitIP: r.ExitIP,
			}); err != nil {
				ctx.Debugf("database: %v", err)
			}
		}
	}

	code, err := router.Run(ctx, opts, args)
	if err != nil {
		if errors.Is(err, router.ErrNoCache) {
			if d != nil {
				ctx.Errorf("no tested proxies in the database (run build, or use -r for fresh)")
			} else {
				ctx.Errorf("no cache at %s (build one, or use -r for fresh)", o.cache)
			}
			return 1
		}
		ctx.Errorf("%v", err)
	}
	return code
}

// askYesNo prompts on w and reads a Y/n answer from r. Defaults to yes on
// empty input; returns false only for an explicit "n".
func askYesNo(r io.Reader, w io.Writer, prompt string) bool {
	fmt.Fprintf(w, "%s [Y/n] ", prompt)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return true // non-interactive: assume yes
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return !strings.HasPrefix(ans, "n")
}
