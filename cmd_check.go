package main

import (
	"fmt"
	"os"

	"github.com/szmelc-inc/silkrouter/internal/cache"
	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/source"
)

var checkOpts struct {
	probe   string
	testURL string
	timeout config.Duration
	tries   int
	jobs    int
	json    bool
}

var probesOpts struct{ json bool }

func init() {
	cli.Register(cli.Command{
		Name:    "check",
		Group:   cli.GroupCheck,
		Summary: "test a single proxy or a file of proxies",
		Description: `Measures liveness and latency with a caching probe. The argument is a
proxy (scheme://host:port, host:port) or a file with one proxy per line.
Exits 1 when nothing is alive.`,
		Usage: "[options] <proxy | file>",
		Args:  cli.ArgsFiles,
		Flags: func(fs *cli.FlagSet) {
			cfg := fs.Config
			o := &checkOpts
			fs.String(&o.probe, "probe", cfg.Probe.Name, "caching probe to use").
				Placeholder("NAME").Dynamic("probes").ConfigKey("probe.name")
			fs.String(&o.testURL, "test-url", cfg.Probe.TestURL, "custom probe URL (any 2xx = alive)").
				Placeholder("URL").ConfigKey("probe.test_url")
			fs.Duration(&o.timeout, "t,timeout", cfg.Check.Timeout, "per-try timeout").ConfigKey("check.timeout")
			fs.Int(&o.tries, "n,tries", cfg.Check.Tries, "samples averaged per proxy").ConfigKey("check.tries")
			fs.Int(&o.jobs, "j,workers,jobs", cfg.Check.Workers, "parallel workers (file mode)").ConfigKey("check.workers")
			fs.Bool(&o.json, "json", false, "print results as JSON")
		},
		Examples: []cli.Example{
			{Cmd: "check socks5://1.2.3.4:1080"},
			{Cmd: "check -probe google -n 3 proxies.txt"},
		},
		Run: runCheck,
	})
	cli.Register(cli.Command{
		Name:    "probes",
		Group:   cli.GroupCheck,
		Summary: "list the available caching probes",
		Flags: func(fs *cli.FlagSet) {
			fs.Bool(&probesOpts.json, "json", false, "print the probes as JSON")
		},
		Run: runProbes,
	})
	cli.RegisterCompleter("probes", func(*cli.Context) []cli.Candidate {
		var out []cli.Candidate
		for _, p := range probe.All() {
			out = append(out, cli.Candidate{Value: p.Name, Desc: p.Desc})
		}
		return out
	})
}

type checkResult struct {
	Proxy     string `json:"proxy"`
	Alive     bool   `json:"alive"`
	LatencyMS int    `json:"latency_ms,omitempty"`
	ExitIP    string `json:"exit_ip,omitempty"`
	Error     string `json:"error,omitempty"`
}

// runCheck tests a single proxy or a whole file of proxies for liveness and
// latency using a caching probe.
func runCheck(ctx *cli.Context, args []string) int {
	o := &checkOpts
	if len(args) != 1 {
		return ctx.UsageError("need exactly one proxy or file")
	}
	target := args[0]

	pr, err := selectProbe(o.probe, o.testURL)
	if err != nil {
		ctx.Errorf("%v", err)
		return 2
	}
	timeout := o.timeout.D()
	c := ctx.Colors()

	// File mode when the argument is an existing readable file.
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		data, err := os.ReadFile(target)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		proxies := source.ParseLines(data)
		if len(proxies) == 0 {
			ctx.Errorf("no valid proxies in %s", target)
			return 1
		}
		res := cache.Build(ctx, proxies, cache.BuildOptions{
			Probe: pr, Jobs: o.jobs, Tries: o.tries, Timeout: timeout, Progress: !ctx.Quiet && !o.json,
		}, os.Stderr)
		code := 0
		if len(res.Working) == 0 {
			code = 1
		}
		if o.json {
			out := struct {
				Tested int           `json:"tested"`
				Alive  []checkResult `json:"alive"`
			}{Tested: res.Tested, Alive: []checkResult{}}
			for _, p := range res.Working {
				out.Alive = append(out.Alive, checkResult{Proxy: p.URL(), Alive: true, LatencyMS: p.LatencyMS})
			}
			if ctx.JSON(out) != 0 {
				return 1
			}
			return code
		}
		fmt.Fprintf(ctx.Stdout, "%d/%d proxies alive\n", len(res.Working), res.Tested)
		for _, p := range res.Working {
			fmt.Fprintf(ctx.Stdout, "%s%-40s%s %dms\n", c.Green, p.URL(), c.Reset, p.LatencyMS)
		}
		return code
	}

	// Single-proxy mode.
	p, err := proxy.Parse(target)
	if err != nil {
		ctx.Errorf("%v", err)
		return 2
	}
	r := pr.Measure(ctx, p, o.tries, timeout)
	if o.json {
		cr := checkResult{Proxy: p.URL(), Alive: r.OK, ExitIP: r.ExitIP}
		if r.OK {
			cr.LatencyMS = r.LatencyMS
		} else if r.Err != nil {
			cr.Error = r.Err.Error()
		}
		if ctx.JSON(cr) != 0 || !r.OK {
			return 1
		}
		return 0
	}
	if !r.OK {
		fmt.Fprintf(ctx.Stdout, "%s%s%s  %sDEAD%s  (%v)\n", c.Bold, p.URL(), c.Reset, c.Red, c.Reset, r.Err)
		return 1
	}
	line := fmt.Sprintf("%s%s%s  %sALIVE%s  %dms", c.Bold, p.URL(), c.Reset, c.Green, c.Reset, r.LatencyMS)
	if r.ExitIP != "" {
		line += fmt.Sprintf("  exit=%s", r.ExitIP)
	}
	fmt.Fprintln(ctx.Stdout, line)
	return 0
}

// runProbes lists the available caching probes.
func runProbes(ctx *cli.Context, _ []string) int {
	if probesOpts.json {
		type row struct {
			Name    string `json:"name"`
			Desc    string `json:"desc"`
			Default bool   `json:"default"`
		}
		out := []row{}
		for _, p := range probe.All() {
			out = append(out, row{Name: p.Name, Desc: p.Desc, Default: p.Name == ctx.Config.Probe.Name})
		}
		return ctx.JSON(out)
	}
	c := ctx.Colors()
	fmt.Fprintf(ctx.Stdout, "Available caching probes (%s build -probe <name>):\n", ctx.Prog)
	for _, p := range probe.All() {
		marker := "  "
		if p.Name == ctx.Config.Probe.Name {
			marker = c.Green + "* " + c.Reset
		}
		fmt.Fprintf(ctx.Stdout, "%s%-12s %s\n", marker, p.Name, p.Desc)
	}
	fmt.Fprintln(ctx.Stdout, "\nOr use -test-url <URL> for a custom probe (any 2xx response = alive).")
	return 0
}
