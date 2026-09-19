package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/scan"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

// cmdBLCheck checks IP(s) against DNS blacklists via mxtoolbox.
func cmdBLCheck(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("blcheck", flag.ContinueOnError)
	var (
		file    = fs.String("f", "", "check every address in FILE (writes FILE-BL)")
		token   = fs.String("token", os.Getenv("MXTB_TOKEN"), "mxtoolbox temp auth token")
		delayS  = fs.Float64("d", 2, "delay between lookups in file mode (seconds)")
		verbose = fs.Bool("v", false, "single-IP mode: list every blacklist")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: silkrouter blcheck [options] <ip>\n       silkrouter blcheck -f <file>\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" && fs.NArg() == 0 {
		fs.Usage()
		return 2
	}

	chk := scan.NewBLChecker(*token, 20*time.Second)
	if err := chk.EnsureToken(ctx); err != nil {
		ui.Errf("silkrouter: %v\n", err)
		return 2
	}
	c := ui.Colors()

	if *file != "" {
		return blFileMode(ctx, chk, *file, time.Duration(*delayS*float64(time.Second)))
	}

	// Single-IP mode.
	ip := scan.ExtractIP(fs.Arg(0))
	if ip == "" {
		ui.Errf("silkrouter: no IPv4 found in %q\n", fs.Arg(0))
		return 1
	}
	res, err := chk.Lookup(ctx, ip)
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
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
	_ = verbose // verbose per-blacklist listing folds into the LISTED names above
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

// cmdProxyCheck checks proxy/anonymity detection via whatismyipaddress.com.
func cmdProxyCheck(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("proxycheck", flag.ContinueOnError)
	var (
		file     = fs.String("f", "", "bulk check every proxy in FILE")
		output   = fs.String("o", "", "output file for -f mode (default: FILE-PX)")
		timeoutS = fs.Int("t", 8, "curl connect/max timeout (seconds)")
		delayS   = fs.Float64("d", 0, "delay between requests in bulk mode (seconds)")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: silkrouter proxycheck [options] <ip|ip:port|proto://ip:port>\n       silkrouter proxycheck -f <file>\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" && fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	chk := scan.PXChecker{Timeout: time.Duration(*timeoutS) * time.Second}
	c := ui.Colors()

	if *file != "" {
		return pxFileMode(ctx, chk, *file, *output, time.Duration(*delayS*float64(time.Second)))
	}

	p, err := scan.ParseTarget(fs.Arg(0))
	if err != nil {
		ui.Errf("silkrouter: %v\n", err)
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
