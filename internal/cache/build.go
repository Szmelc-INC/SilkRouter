package cache

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

// BuildOptions configures a cache build run.
type BuildOptions struct {
	Probe    probe.Probe
	Jobs     int           // concurrent workers
	Tries    int           // samples averaged per proxy
	Timeout  time.Duration // per-try timeout
	ShowAll  bool          // also log DEAD proxies
	Progress bool          // draw the live progress line on w
}

// BuildResult reports what a build produced.
type BuildResult struct {
	Tested  int
	Working []proxy.Proxy
}

// Build tests every candidate through opts.Probe using a bounded worker pool
// and returns the working proxies (with measured latency). Progress is drawn to
// w when opts.Progress is set. The context aborts the run early if cancelled.
func Build(ctx context.Context, candidates []proxy.Proxy, opts BuildOptions, w io.Writer) BuildResult {
	if opts.Jobs < 1 {
		opts.Jobs = 1
	}
	if opts.Tries < 1 {
		opts.Tries = 1
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}

	total := len(candidates)
	jobs := make(chan proxy.Proxy)
	results := make(chan probe.Result)
	working := make(chan proxy.Proxy, opts.Jobs)

	// Feed candidates.
	go func() {
		defer close(jobs)
		for _, p := range candidates {
			select {
			case <-ctx.Done():
				return
			case jobs <- p:
			}
		}
	}()

	// Worker pool.
	var wg sync.WaitGroup
	for i := 0; i < opts.Jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				res := opts.Probe.Measure(ctx, p, opts.Tries, opts.Timeout)
				if res.OK {
					p.LatencyMS = res.LatencyMS
					working <- p
				}
				select {
				case results <- res:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Close channels once all workers finish.
	go func() {
		wg.Wait()
		close(results)
		close(working)
	}()

	// Collect working proxies concurrently with progress reporting.
	var collected []proxy.Proxy
	done := make(chan struct{})
	go func() {
		for p := range working {
			collected = append(collected, p)
		}
		close(done)
	}()

	// Progress: consume results, draw the live line.
	c := ui.Colors()
	var tested, ok int
	for res := range results {
		tested++
		if res.OK {
			ok++
		} else if opts.ShowAll && opts.Progress {
			fmt.Fprintf(w, "\r\033[2K%sDEAD%s  (%v)\n", c.Red, c.Reset, res.Err)
		}
		if opts.Progress {
			fmt.Fprintf(w, "\r%stested %d/%d  working %d%s", c.Dim, tested, total, ok, c.Reset)
		}
	}
	if opts.Progress {
		fmt.Fprintln(w)
	}
	<-done

	Sort(collected)
	return BuildResult{Tested: tested, Working: collected}
}
