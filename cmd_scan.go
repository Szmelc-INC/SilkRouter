package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/scan"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

var blOpts struct {
	file    string
	token   string
	delay   config.Duration
	timeout config.Duration
	verbose bool
}

var pxOpts struct {
	file    string
	output  string
	timeout config.Duration
	delay   config.Duration
}

func init() {
	cli.Register(cli.Command{
		Name:    "blcheck",
		Group:   cli.GroupCheck,
		Summary: "check IP(s) against ~60 DNS blacklists (mxtoolbox)",
		Description: `Looks an IPv4 address up on mxtoolbox's DNS blacklist check. With -f FILE,
every address found in FILE is checked and the results are written to
FILE-BL. A temporary API token is fetched automatically unless -token (or
$MXTB_TOKEN) is given.`,
		Usage: "[options] <ip>\n-f <file>",
		Flags: func(fs *cli.FlagSet) {
			cfg := fs.Config
			fs.String(&blOpts.file, "f,file", "", "check every address in FILE (writes FILE-BL)").
				Placeholder("FILE").Files()
			fs.String(&blOpts.token, "token", cfg.BLCheck.Token, "mxtoolbox temp auth token").
				Placeholder("TOKEN").ConfigKey("blcheck.token").NoDefault()
			fs.Duration(&blOpts.delay, "d,delay", cfg.BLCheck.Delay, "delay between lookups in file mode").
				ConfigKey("blcheck.delay")
			fs.Duration(&blOpts.timeout, "timeout", cfg.BLCheck.Timeout, "HTTP timeout per lookup").
				ConfigKey("blcheck.timeout")
			fs.Bool(&blOpts.verbose, "v,verbose", false, "single-IP mode: list every blacklist")
		},
		Examples: []cli.Example{
			{Cmd: "blcheck 8.8.8.8"},
			{Cmd: "blcheck -f proxies.txt -d 1"},
		},
		Run: runBLCheck,
	})
	cli.Register(cli.Command{
		Name:    "proxycheck",
		Group:   cli.GroupCheck,
		Summary: "check proxy/anonymity detection (whatismyipaddress)",
		Description: `Asks whatismyipaddress.com's proxy check whether the address looks like
a proxy. Single target: ip, ip:port or scheme://ip:port. With -f FILE,
every proxy in FILE is checked and the verdicts are written to FILE-PX
(or -o OUTPUT).`,
		Usage: "[options] <ip | ip:port | scheme://ip:port>\n-f <file> [-o <output>]",
		Flags: func(fs *cli.FlagSet) {
			cfg := fs.Config
			fs.String(&pxOpts.file, "f,file", "", "bulk check every proxy in FILE").Placeholder("FILE").Files()
			fs.String(&pxOpts.output, "o,output", "", "output file for -f mode (default: FILE-PX)").
				Placeholder("FILE").Files()
			fs.Duration(&pxOpts.timeout, "t,timeout", cfg.ProxyCheck.Timeout, "connect / request timeout").
				ConfigKey("proxycheck.timeout")
			fs.Duration(&pxOpts.delay, "d,delay", cfg.ProxyCheck.Delay, "delay between requests in bulk mode").
				ConfigKey("proxycheck.delay")
		},
		Examples: []cli.Example{
			{Cmd: "proxycheck socks5://1.2.3.4:1080"},
			{Cmd: "proxycheck -f proxies.txt -d 2"},
		},
		Run: runProxyCheck,
	})
}

// runBLCheck checks IP(s) against DNS blacklists via mxtoolbox.
func runBLCheck(ctx *cli.Context, args []string) int {
	if blOpts.file == "" && len(args) == 0 {
		return ctx.UsageError("need an IP address or -f FILE")
	}

	chk := scan.NewBLChecker(blOpts.token, blOpts.timeout.D())
	if err := chk.EnsureToken(ctx); err != nil {
		ctx.Errorf("%v", err)
		return 2
	}
	c := ui.Colors()

	if blOpts.file != "" {
		return blFileMode(ctx, chk, blOpts.file, blOpts.delay.D())
	}

	// Single-IP mode.
	ip := scan.ExtractIP(args[0])
	if ip == "" {
		ctx.Errorf("no IPv4 found in %q", args[0])
		return 1
	}
	res, err := chk.Lookup(ctx, ip)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	if !res.Parsed {
		fmt.Printf("%s?%s %s%s%s  API error / no result (token expired? try -token)\n", c.Yellow, c.Reset, c.Bold, ip, c.Reset)
		return 3
	}
	if res.Listed > 0 {
		fmt.Printf("%s%s%s  %s[%d/%d]%s  %sLISTED%s\n", c.Red, ip, c.Reset, c.Bold, res.Listed, res.Total, c.Reset, c.Red, c.Reset)
		for _, n := range res.Names {
			fmt.Printf("  %s✗%s %s\n", c.Red, c.Reset, n)
		}
	} else {
		fmt.Printf("%s%s%s  %s[0/%d]%s  %sclean%s\n", c.Green, ip, c.Reset, c.Bold, res.Total, c.Reset, c.Green, c.Reset)
	}
	_ = blOpts.verbose || ctx.Verbose // per-blacklist listing folds into the LISTED names above
	return 0
}

func blFileMode(ctx context.Context, chk *scan.BLChecker, path string, delay time.Duration) int {
	in, err := os.Open(path)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
		return 1
	}
	defer in.Close()
	outPath := path + "-BL"
	out, err := os.Create(outPath)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
		return 1
	}
	defer out.Close()
	c := ui.Colors()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	i := 0
	for sc.Scan() {
		line := sc.Text()
		ip := scan.ExtractIP(line)
		if ip == "" {
			fmt.Fprintln(out, line) // passthrough
			continue
		}
		i++
		res, err := chk.Lookup(ctx, ip)
		if err != nil || !res.Parsed {
			fmt.Fprintf(out, "%s [Blacklisted: ERROR]\n", line)
			ui.Errf("%s[%d]%s %s%-21s%s %sERROR%s\n", c.Dim, i, c.Reset, c.Bold, ip, c.Reset, c.Yellow, c.Reset)
			sleepCtx(ctx, delay)
			continue
		}
		names := strings.Join(res.Names, ", ")
		fmt.Fprintf(out, "%s [Blacklisted: %d/%d [%s]]\n", line, res.Listed, res.Total, names)
		if res.Listed > 0 {
			ui.Errf("%s[%d]%s %s%-21s%s %s%d/%d%s %s\n", c.Dim, i, c.Reset, c.Bold, ip, c.Reset, c.Red, res.Listed, res.Total, c.Reset, names)
		} else {
			ui.Errf("%s[%d]%s %s%-21s%s %s0/%d clean%s\n", c.Dim, i, c.Reset, c.Bold, ip, c.Reset, c.Green, res.Total, c.Reset)
		}
		sleepCtx(ctx, delay)
	}
	ui.Errf("\n%sDone.%s Results written to %s%s%s\n", c.Green, c.Reset, c.Bold, outPath, c.Reset)
	return 0
}

// runProxyCheck checks proxy/anonymity detection via whatismyipaddress.com.
func runProxyCheck(ctx *cli.Context, args []string) int {
	if pxOpts.file == "" && len(args) == 0 {
		return ctx.UsageError("need a target or -f FILE")
	}
	chk := scan.PXChecker{Timeout: pxOpts.timeout.D()}
	c := ui.Colors()

	if pxOpts.file != "" {
		return pxFileMode(ctx, chk, pxOpts.file, pxOpts.output, pxOpts.delay.D())
	}

	p, err := scan.ParseTarget(args[0])
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	res := chk.Check(ctx, p)
	printPXVerdict(p, res)
	if res.Status == scan.PXDetected {
		fmt.Printf("\n%s[found 1/1]%s\n", c.Red, c.Reset)
	} else {
		fmt.Printf("\n%s[not found 0/1]%s\n", c.Green, c.Reset)
	}
	return 0
}

func pxFileMode(ctx context.Context, chk scan.PXChecker, path, outPath string, delay time.Duration) int {
	data, err := os.ReadFile(path)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
		return 1
	}
	if outPath == "" {
		outPath = path + "-PX"
	}
	// Parse valid targets, preserving original lines for the output file.
	type entry struct {
		orig string
		p    proxy.Proxy
	}
	var entries []entry
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if p, err := proxy.Parse(line); err == nil {
			entries = append(entries, entry{orig: line, p: p})
		}
	}
	if len(entries) == 0 {
		ui.Errf("silkrouter: no valid entries in %s\n", path)
		return 1
	}
	out, err := os.Create(outPath)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
		return 1
	}
	defer out.Close()
	c := ui.Colors()

	var detected, clean, blocked, failed int
	for i, e := range entries {
		res := chk.Check(ctx, e.p)
		color := c.Yellow
		switch res.Status {
		case scan.PXDetected:
			color = c.Red
			detected++
		case scan.PXClean:
			color = c.Green
			clean++
		case scan.PXBlocked:
			blocked++
		case scan.PXFail:
			failed++
		}
		fmt.Printf("[%d/%d] %-32s -> %s%s%s\n", i+1, len(entries), e.p.URL(), color, res.Status, c.Reset)
		seen := ""
		if res.SeenIP != "" {
			seen = " (seen: " + res.SeenIP + ")"
		}
		fmt.Fprintf(out, "%s => %s%s\n", e.orig, res.Status, seen)
		sleepCtx(ctx, delay)
	}
	fmt.Printf("\n%sSummary:%s %d checked | %s%d detected%s | %s%d clean%s | %s%d blocked%s | %s%d failed%s\n",
		c.Bold, c.Reset, len(entries), c.Red, detected, c.Reset, c.Green, clean, c.Reset, c.Yellow, blocked, c.Reset, c.Yellow, failed, c.Reset)
	fmt.Printf("Results written to: %s%s%s\n", c.Cyan, outPath, c.Reset)
	return 0
}

func printPXVerdict(p proxy.Proxy, res scan.PXResult) {
	c := ui.Colors()
	fmt.Printf("%sTarget:%s      %s\n", c.Bold, c.Reset, p.URL())
	switch res.Status {
	case scan.PXDetected:
		fmt.Printf("%sVerdict:%s     %sPROXY DETECTED%s\n", c.Bold, c.Reset, c.Red, c.Reset)
	case scan.PXClean:
		fmt.Printf("%sVerdict:%s     %sNO PROXY DETECTED%s\n", c.Bold, c.Reset, c.Green, c.Reset)
	case scan.PXBlocked:
		fmt.Printf("%sVerdict:%s     %sBLOCKED (Cloudflare challenge)%s\n", c.Bold, c.Reset, c.Yellow, c.Reset)
	case scan.PXFail:
		fmt.Printf("%sVerdict:%s     %sCONNECTION FAILED%s\n", c.Bold, c.Reset, c.Yellow, c.Reset)
	default:
		fmt.Printf("%sVerdict:%s     %sUNKNOWN%s\n", c.Bold, c.Reset, c.Yellow, c.Reset)
	}
	if res.SeenIP != "" {
		fmt.Printf("%sSeen IP:%s     %s\n", c.Bold, c.Reset, res.SeenIP)
	}
	if res.WIMIA != "" {
		fmt.Printf("%srDNS Test:%s   %s\n", c.Bold, c.Reset, colorizeBool(res.RDNS))
		fmt.Printf("%sWIMIA Test:%s  %s\n", c.Bold, c.Reset, colorizeBool(res.WIMIA))
		fmt.Printf("%sTor Test:%s    %s\n", c.Bold, c.Reset, colorizeBool(res.Tor))
		fmt.Printf("%sLoc Test:%s    %s\n", c.Bold, c.Reset, colorizeBool(res.Loc))
		fmt.Printf("%sHeader Test:%s %s\n", c.Bold, c.Reset, colorizeBool(res.Header))
	}
}

func colorizeBool(v string) string {
	c := ui.Colors()
	switch v {
	case "TRUE":
		return c.Red + v + c.Reset
	case "FALSE":
		return c.Green + v + c.Reset
	default:
		return c.Yellow + "n/a" + c.Reset
	}
}

// sleepCtx sleeps for d unless the context is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
