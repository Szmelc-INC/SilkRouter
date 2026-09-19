// Package cache owns the on-disk proxy cache: loading, saving, merging and
// reporting. The file format matches the original tool so caches remain
// interchangeable:  scheme://host:port [Nms]  sorted fastest -> slowest.
package cache

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
)

// DefaultPath returns the cache location, honouring $PX_LIST, else a temp file.
func DefaultPath() string {
	if v := os.Getenv("PX_LIST"); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "silkrouter-proxy.list")
}

// Load parses the cache file, skipping blank/comment/invalid lines. A missing
// file returns (nil, os.ErrNotExist) so callers can offer to build one.
func Load(path string) ([]proxy.Proxy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []proxy.Proxy
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := proxy.Parse(line)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("read cache %s: %w", path, err)
	}
	return out, nil
}

// Sort orders proxies fastest -> slowest (unknown latency sinks to the end).
func Sort(ps []proxy.Proxy) {
	sort.SliceStable(ps, func(i, j int) bool {
		li, lj := ps[i].LatencyMS, ps[j].LatencyMS
		if li == 0 {
			li = 1 << 30
		}
		if lj == 0 {
			lj = 1 << 30
		}
		return li < lj
	})
}

// Save writes proxies (sorted) to path atomically via a temp file + rename.
func Save(path string, ps []proxy.Proxy) error {
	Sort(ps)
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".silkrouter-*")
	if err != nil {
		return fmt.Errorf("create temp cache: %w", err)
	}
	tmpName := tmp.Name()
	w := bufio.NewWriter(tmp)
	for _, p := range ps {
		fmt.Fprintln(w, p.String())
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close cache: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("replace cache: %w", err)
	}
	return nil
}

// Merge combines existing and fresh proxies, de-duplicating by identity. Fresh
// measurements win on collision, and the result is sorted fastest -> slowest.
func Merge(existing, fresh []proxy.Proxy) []proxy.Proxy {
	byKey := make(map[string]proxy.Proxy, len(existing)+len(fresh))
	order := make([]string, 0, len(existing)+len(fresh))
	add := func(p proxy.Proxy) {
		if _, ok := byKey[p.Key()]; !ok {
			order = append(order, p.Key())
		}
		byKey[p.Key()] = p
	}
	for _, p := range existing {
		add(p)
	}
	for _, p := range fresh { // appended last => new timings win
		add(p)
	}
	out := make([]proxy.Proxy, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	Sort(out)
	return out
}

// Report prints a human summary of the cache to w (mirrors px.sh -L).
func Report(w io.Writer, path string, ps []proxy.Proxy) {
	fmt.Fprintf(w, "# Cache: %s\n", path)
	fmt.Fprintf(w, "Proxies\n* total: [%d]\n", len(ps))

	byScheme := map[proxy.Scheme]int{}
	var b100, s01, s15, s510, s10 int
	for _, p := range ps {
		byScheme[p.Scheme]++
		d := p.LatencyMS
		switch {
		case d > 0 && d < 100:
			b100++
			s01++
		case d <= 1000:
			s01++
		case d <= 5000:
			s15++
		case d <= 10000:
			s510++
		default:
			s10++
		}
	}
	for _, s := range proxy.AllSchemes {
		if n := byScheme[s]; n > 0 {
			fmt.Fprintf(w, "* %s: [%d]\n", s, n)
		}
	}
	fmt.Fprintf(w, "\nDelay\n")
	fmt.Fprintf(w, "* Below 100ms: [%d]\n", b100)
	fmt.Fprintf(w, "* 0-1s: [%d]\n", s01)
	fmt.Fprintf(w, "* 1-5s: [%d]\n", s15)
	fmt.Fprintf(w, "* 5-10s: [%d]\n", s510)
	fmt.Fprintf(w, "* 10s+: [%d]\n", s10)
}
