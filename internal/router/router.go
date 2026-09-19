// Package router selects a proxy and runs a command through it by exporting the
// standard proxy environment variables, mirroring the original px.sh run mode.
package router

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cache"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/source"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

// ErrNoCache is returned when the cache is missing and fresh mode is off, so
// the CLI can offer to build one.
var ErrNoCache = errors.New("no proxy cache")

// Verify policy constants.
const (
	VerifyAuto = -1
	VerifySkip = 0
	VerifyDo   = 1
)

// Options controls proxy selection and command execution.
type Options struct {
	CachePath     string
	Force         string // -p: a specific proxy string (skips list & filters)
	MinMS, MaxMS  int
	Want          proxy.Scheme // "" = any, else filter (http/https/socks*)
	SocksOnly     bool
	HTTPOnly      bool
	Verify        int // VerifyAuto/Skip/Do
	VerifyTimeout time.Duration
	Fresh         bool
	Verbose       bool
	Probe         probe.Probe
	Origin        source.Origin
}

// Run picks a proxy per opts and executes args through it. It returns the
// command's exit code (or a non-nil error for setup failures before exec).
func Run(ctx context.Context, opts Options, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("need a command to run (or use the build subcommand)")
	}

	cand, err := selectCandidates(ctx, opts)
	if err != nil {
		return 1, err
	}
	if len(cand) == 0 {
		return 1, fmt.Errorf("no proxies match (cache=%s %d-%dms want=%s)",
			opts.CachePath, opts.MinMS, opts.MaxMS, wantLabel(opts))
	}

	// Auto verify policy: fresh proxies are untested -> verify; cache -> trust.
	verify := opts.Verify
	if verify == VerifyAuto {
		if opts.Fresh {
			verify = VerifyDo
		} else {
			verify = VerifySkip
		}
	}

	chosen, exitIP, err := choose(ctx, opts, cand, verify)
	if err != nil {
		return 1, err
	}

	if opts.Verbose {
		if exitIP == "" {
			exitIP = probeExitIP(ctx, chosen, opts.VerifyTimeout)
		}
		if exitIP != "" {
			ui.Errf("[silkrouter] exit IP via %s -> %s\n", chosen.URL(), exitIP)
		}
	}

	return runCommand(ctx, chosen, args)
}

// selectCandidates gathers the pool of proxies to consider, before filtering.
func selectCandidates(ctx context.Context, opts Options) ([]proxy.Proxy, error) {
	if opts.Force != "" {
		p, err := proxy.Parse(opts.Force)
		if err != nil {
			return nil, fmt.Errorf("bad -p proxy %q: %w", opts.Force, err)
		}
		return []proxy.Proxy{p}, nil
	}

	var pool []proxy.Proxy
	if opts.Fresh {
		ps, err := opts.Origin.Candidates(ctx, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("fetch fresh proxies: %w", err)
		}
		pool = ps
	} else {
		ps, err := cache.Load(opts.CachePath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, ErrNoCache
			}
			return nil, err
		}
		pool = ps
	}

	// Latency window (cache entries carry latency; fresh ones have 0 = keep).
	filtered := pool[:0:0]
	for _, p := range pool {
		d := p.LatencyMS
		if d == 0 && !opts.Fresh {
			// unknown-latency cache entry: keep only if window starts at 0
			if opts.MinMS > 0 {
				continue
			}
		} else if !opts.Fresh {
			if d < opts.MinMS || (opts.MaxMS > 0 && d > opts.MaxMS) {
				continue
			}
		}
		if !schemeAllowed(p.Scheme, opts) {
			continue
		}
		filtered = append(filtered, p)
	}

	rand.Shuffle(len(filtered), func(i, j int) {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	})
	return filtered, nil
}

// schemeAllowed applies the -H/-s protocol filters.
func schemeAllowed(s proxy.Scheme, opts Options) bool {
	switch {
	case opts.HTTPOnly:
		return s == proxy.HTTP || s == proxy.HTTPS
	case opts.SocksOnly:
		return s.IsSocks()
	default:
		return true
	}
}

func wantLabel(opts Options) string {
	switch {
	case opts.HTTPOnly:
		return "http"
	case opts.SocksOnly:
		return "socks"
	default:
		return "any"
	}
}

// choose picks the proxy to use. When verifying, it walks candidates until one
// answers the probe; otherwise it takes the first (already shuffled) candidate.
func choose(ctx context.Context, opts Options, cand []proxy.Proxy, verify int) (proxy.Proxy, string, error) {
	if opts.Force != "" || verify == VerifySkip {
		return cand[0], "", nil
	}
	var lastErr error
	for _, p := range cand {
		res := opts.Probe.Measure(ctx, p, 1, opts.VerifyTimeout)
		if res.OK {
			return p, res.ExitIP, nil
		}
		lastErr = res.Err
	}
	return proxy.Proxy{}, "", fmt.Errorf("all %d candidate proxies failed verification (last: %v)", len(cand), lastErr)
}

// probeExitIP fetches the exit IP through p using an IP-echo probe.
func probeExitIP(ctx context.Context, p proxy.Proxy, timeout time.Duration) string {
	pr, err := probe.Get("ipify")
	if err != nil {
		return ""
	}
	return pr.Measure(ctx, p, 1, timeout).ExitIP
}

// runCommand runs args with proxy env vars set, forwarding std streams, and
// returns the child's exit code.
func runCommand(ctx context.Context, p proxy.Proxy, args []string) (int, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = proxyEnv(os.Environ(), p.URL())

	short := args[0]
	if i := strings.LastIndexAny(short, `/\`); i >= 0 {
		short = short[i+1:]
	}
	ui.Errf("[silkrouter] proxy: %s %s  |  cmd: %s\n", p.Scheme, p.Addr(), short)

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 127, fmt.Errorf("run %q: %w", args[0], err)
	}
	return 0, nil
}

// proxyEnv returns env with the standard proxy variables (upper+lower case) set
// to url, replacing any pre-existing ones.
func proxyEnv(base []string, url string) []string {
	keys := []string{"ALL_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "all_proxy", "http_proxy", "https_proxy"}
	skip := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		skip[k] = struct{}{}
	}
	out := make([]string, 0, len(base)+len(keys))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			if _, ok := skip[kv[:i]]; ok {
				continue
			}
		}
		out = append(out, kv)
	}
	for _, k := range keys {
		out = append(out, k+"="+url)
	}
	return out
}
