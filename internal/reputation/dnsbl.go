package reputation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// DNSBL queries DNS blacklists natively: a.b.c.d is looked up as
// d.c.b.a.<zone>; an answer in 127.0.0.0/8 means "listed", NXDOMAIN means
// "not listed".
type DNSBL struct {
	Zones   []string
	Timeout time.Duration // per lookup (0 = 4s)
	// Lookup resolves a name (nil = net.DefaultResolver.LookupHost); tests
	// replace it.
	Lookup func(ctx context.Context, host string) ([]string, error)
	// Parallel bounds concurrent lookups per Check (0 = 8).
	Parallel int
}

// DNSBLResult is one zone's verdict.
type DNSBLResult struct {
	Zone   string   `json:"zone"`
	Listed bool     `json:"listed"`
	Codes  []string `json:"codes,omitempty"`
	Err    error    `json:"-"`
}

// ErrRefused marks a zone that refused to answer (e.g. Spamhaus through a
// public resolver returns 127.255.255.x).
var ErrRefused = errors.New("query refused by the blacklist (public resolver?)")

// ReverseName returns the DNSBL query name of ip in zone ("" for IPv6, which
// is not supported by most lists).
func ReverseName(ip netip.Addr, zone string) string {
	ip = ip.Unmap()
	if !ip.Is4() {
		return ""
	}
	b := ip.As4()
	return fmt.Sprintf("%d.%d.%d.%d.%s", b[3], b[2], b[1], b[0], strings.TrimSuffix(zone, "."))
}

// Check queries every zone for ip.
func (c *DNSBL) Check(ctx context.Context, ip netip.Addr) []DNSBLResult {
	out := make([]DNSBLResult, len(c.Zones))
	par := c.Parallel
	if par <= 0 {
		par = 8
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	lookup := c.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, zone := range c.Zones {
		wg.Add(1)
		go func(i int, zone string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := DNSBLResult{Zone: zone}
			name := ReverseName(ip, zone)
			if name == "" {
				res.Err = fmt.Errorf("%s: only IPv4 is supported", zone)
				out[i] = res
				return
			}
			lctx, cancel := context.WithTimeout(ctx, timeout)
			addrs, err := lookup(lctx, name)
			cancel()
			if err != nil {
				var dnsErr *net.DNSError
				if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
					out[i] = res // NXDOMAIN: not listed
					return
				}
				res.Err = fmt.Errorf("%s: %w", zone, err)
				out[i] = res
				return
			}
			for _, a := range addrs {
				if strings.HasPrefix(a, "127.255.255.") {
					res.Err = fmt.Errorf("%s: %w (%s)", zone, ErrRefused, a)
					break
				}
				if strings.HasPrefix(a, "127.") {
					res.Listed = true
					res.Codes = append(res.Codes, a)
				}
			}
			if res.Err != nil {
				res.Listed, res.Codes = false, nil
			}
			sort.Strings(res.Codes)
			out[i] = res
		}(i, zone)
	}
	wg.Wait()
	return out
}

// Cache memoises per-IP answers for the duration of one scan (many proxies
// share an exit IP). Safe for concurrent use; concurrent callers for the
// same key wait for the first one.
type Cache[T any] struct {
	mu sync.Mutex
	m  map[string]*cacheEntry[T]
}

type cacheEntry[T any] struct {
	once sync.Once
	v    T
}

// Get returns the cached value for key, computing it with fn once.
func (c *Cache[T]) Get(key string, fn func() T) (T, bool) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*cacheEntry[T]{}
	}
	e, hit := c.m[key]
	if !hit {
		e = &cacheEntry[T]{}
		c.m[key] = e
	}
	c.mu.Unlock()
	e.once.Do(func() { e.v = fn() })
	return e.v, hit
}
