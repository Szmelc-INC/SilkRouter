// Package reputation answers "how known / how dirty is this IP": public
// IP lists (known proxies, Tor exits, VPN / datacenter ranges, abuse
// feeds) downloaded into <home>/lists, DNS blacklists queried natively, and
// an ip-api compatible geolocation client. Nothing here touches the
// database; scans turn the answers into reputation rows.
package reputation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// List kinds (they match db.ListKinds).
const (
	KindIPSet      = "ipset"      // abuse / attack feeds
	KindProxyList  = "proxylist"  // known open proxies, anonymizers, VPNs
	KindTorExit    = "torexit"    // Tor exit relays
	KindDatacenter = "datacenter" // hosting / datacenter ranges
	KindDNSBL      = "dnsbl"
	KindAPI        = "api"
)

// List is one downloadable IP list.
type List struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	URL  string `json:"url"`
	Desc string `json:"desc"`
}

const firehol = "https://raw.githubusercontent.com/firehol/blocklist-ipsets/master/"

// Builtin lists. Sites commonly block addresses found on these, so a proxy
// listed here is "known".
var Builtin = []List{
	{"tor-exits", KindTorExit, "https://check.torproject.org/torbulkexitlist", "Tor Project: current Tor exit relays"},
	{"firehol-proxies", KindProxyList, firehol + "firehol_proxies.netset", "FireHOL: open proxies seen by proxy-list sites (large)"},
	{"socks-proxy", KindProxyList, firehol + "socks_proxy_30d.ipset", "socks-proxy.net: SOCKS proxies (30 days)"},
	{"sslproxies", KindProxyList, firehol + "sslproxies_30d.ipset", "sslproxies.org: HTTPS proxies (30 days)"},
	{"speedx-http", KindProxyList, speedx + "http.txt", "TheSpeedX/PROXY-List: public HTTP proxies"},
	{"speedx-socks4", KindProxyList, speedx + "socks4.txt", "TheSpeedX/PROXY-List: public SOCKS4 proxies"},
	{"speedx-socks5", KindProxyList, speedx + "socks5.txt", "TheSpeedX/PROXY-List: public SOCKS5 proxies"},
	{"monosans", KindProxyList, "https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/all.txt", "monosans/proxy-list: checked public proxies"},
	{"proxifly", KindProxyList, "https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt", "proxifly/free-proxy-list: public proxies"},
	{"jetkai", KindProxyList, "https://raw.githubusercontent.com/jetkai/proxy-list/main/online-proxies/txt/proxies.txt", "jetkai/proxy-list: online public proxies"},
	{"clarketm", KindProxyList, "https://raw.githubusercontent.com/clarketm/proxy-list/master/proxy-list-raw.txt", "clarketm/proxy-list: public proxies"},
	{"x4b-vpn", KindProxyList, "https://raw.githubusercontent.com/X4BNet/lists_vpn/main/output/vpn/ipv4.txt", "X4BNet: VPN provider ranges"},
	{"x4b-datacenter", KindDatacenter, "https://raw.githubusercontent.com/X4BNet/lists_vpn/main/output/datacenter/ipv4.txt", "X4BNet: datacenter / hosting ranges"},
	{"firehol-level1", KindIPSet, firehol + "firehol_level1.netset", "FireHOL level 1: attacks, malware, bogons"},
	{"firehol-level2", KindIPSet, firehol + "firehol_level2.netset", "FireHOL level 2: attacks seen in the last 48h"},
	{"firehol-abusers-1d", KindIPSet, firehol + "firehol_abusers_1d.netset", "FireHOL: abusers (1 day)"},
	{"stopforumspam-7d", KindIPSet, firehol + "stopforumspam_7d.ipset", "StopForumSpam: spammers (7 days)"},
	{"cleantalk-7d", KindIPSet, firehol + "cleantalk_7d.ipset", "CleanTalk: spam bots (7 days)"},
	{"blocklist-de", KindIPSet, firehol + "blocklist_de.ipset", "blocklist.de: attackers reported by fail2ban"},
	{"spamhaus-drop", KindIPSet, "https://www.spamhaus.org/drop/drop.txt", "Spamhaus DROP: hijacked / criminal netblocks"},
}

const speedx = "https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/"

// ParseCustom parses "NAME|KIND|URL" (scan.custom_lists).
func ParseCustom(s string) (List, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 3 {
		return List{}, fmt.Errorf("custom list %q: want NAME|KIND|URL", s)
	}
	l := List{Name: strings.TrimSpace(parts[0]), Kind: strings.TrimSpace(parts[1]), URL: strings.TrimSpace(parts[2]), Desc: "custom list"}
	switch l.Kind {
	case KindIPSet, KindProxyList, KindTorExit, KindDatacenter:
	default:
		return List{}, fmt.Errorf("custom list %q: kind %q (want ipset, proxylist, torexit or datacenter)", s, l.Kind)
	}
	if l.Name == "" || !strings.HasPrefix(l.URL, "http") {
		return List{}, fmt.Errorf("custom list %q: needs a name and an http(s) URL", s)
	}
	return l, nil
}

// Select returns the lists named in names (all of all when names is
// empty); unknown names are errors.
func Select(all []List, names []string) ([]List, error) {
	if len(names) == 0 {
		return all, nil
	}
	by := map[string]List{}
	for _, l := range all {
		by[l.Name] = l
	}
	var out []List
	for _, n := range names {
		l, ok := by[strings.TrimSpace(n)]
		if !ok {
			return nil, fmt.Errorf("unknown list %q (px lists shows them)", n)
		}
		out = append(out, l)
	}
	return out, nil
}

// ---- parsed sets -------------------------------------------------------------

type r32 struct{ lo, hi uint32 }

// Set is a parsed list: IPv4 ranges (merged, sorted) plus IPv6 prefixes.
type Set struct {
	List
	FetchedAt time.Time
	Entries   int
	v4        []r32
	v6        []netip.Prefix
}

// Contains reports whether ip is on the list.
func (s *Set) Contains(ip netip.Addr) bool {
	if s == nil || !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if ip.Is4() {
		b := ip.As4()
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		i := sort.Search(len(s.v4), func(i int) bool { return s.v4[i].hi >= v })
		return i < len(s.v4) && s.v4[i].lo <= v
	}
	for _, p := range s.v6 {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// listToken normalises one list entry: scheme:// and user:pass@ are
// dropped and ip:port / [v6]:port lose the port.
func listToken(tok string) string {
	if i := strings.Index(tok, "://"); i >= 0 {
		tok = tok[i+3:]
	}
	if i := strings.LastIndex(tok, "@"); i >= 0 {
		tok = tok[i+1:]
	}
	if ap, err := netip.ParseAddrPort(tok); err == nil {
		return ap.Addr().String()
	}
	return tok
}

// Parse reads a list in any of the common formats: one IP, CIDR, ip:port
// or scheme://[user:pass@]ip:port per line, comments after '#' or ';',
// extra columns ignored ("1.2.0.0/16 ; SBL1").
func Parse(l List, r io.Reader) (*Set, error) {
	s := &Set{List: l}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		tok := listToken(f[0])
		if p, err := netip.ParsePrefix(tok); err == nil {
			s.add(p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(tok); err == nil {
			s.add(netip.PrefixFrom(a, a.BitLen()))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	s.finish()
	return s, nil
}

func (s *Set) add(p netip.Prefix) {
	s.Entries++
	a := p.Addr().Unmap()
	if a.Is4() {
		bits := p.Bits()
		if p.Addr().Is4In6() {
			bits -= 96
		}
		b := a.As4()
		lo := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		var size uint32 = 0xffffffff
		if bits > 0 {
			size = (uint32(1) << (32 - uint(bits))) - 1
		}
		if bits == 32 {
			size = 0
		}
		s.v4 = append(s.v4, r32{lo, lo + size})
		return
	}
	s.v6 = append(s.v6, p)
}

func (s *Set) finish() {
	sort.Slice(s.v4, func(i, j int) bool { return s.v4[i].lo < s.v4[j].lo })
	merged := s.v4[:0]
	for _, r := range s.v4 {
		if n := len(merged); n > 0 && r.lo <= merged[n-1].hi+1 && merged[n-1].hi != 0xffffffff {
			if r.hi > merged[n-1].hi {
				merged[n-1].hi = r.hi
			}
			continue
		}
		merged = append(merged, r)
	}
	s.v4 = merged
}

// ---- download cache ----------------------------------------------------------

// Meta is the sidecar of a downloaded list (<dir>/<name>.json).
type Meta struct {
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	URL          string    `json:"url"`
	FetchedAt    time.Time `json:"fetched_at,omitzero"`
	CheckedAt    time.Time `json:"checked_at,omitzero"`
	Entries      int       `json:"entries"`
	Bytes        int64     `json:"bytes"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// Manager downloads, caches and parses lists.
type Manager struct {
	Dir    string        // <home>/lists
	TTL    time.Duration // re-download when older (0 = 12h)
	Client *http.Client
	Lists  []List
}

func (m *Manager) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (m *Manager) ttl() time.Duration {
	if m.TTL <= 0 {
		return 12 * time.Hour
	}
	return m.TTL
}

func (m *Manager) dataPath(name string) string { return filepath.Join(m.Dir, name+".txt") }
func (m *Manager) metaPath(name string) string { return filepath.Join(m.Dir, name+".json") }

// ReadMeta returns the sidecar of a list (zero Meta when never fetched).
func (m *Manager) ReadMeta(l List) Meta {
	meta := Meta{Name: l.Name, Kind: l.Kind, URL: l.URL}
	data, err := os.ReadFile(m.metaPath(l.Name))
	if err != nil {
		return meta
	}
	_ = json.Unmarshal(data, &meta)
	meta.Name, meta.Kind, meta.URL = l.Name, l.Kind, l.URL
	return meta
}

func (m *Manager) writeMeta(meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(m.metaPath(meta.Name), data)
}

// Status returns the sidecar of every managed list.
func (m *Manager) Status() []Meta {
	out := make([]Meta, 0, len(m.Lists))
	for _, l := range m.Lists {
		out = append(out, m.ReadMeta(l))
	}
	return out
}

// Fresh reports whether the local copy of l is younger than the TTL.
func (m *Manager) Fresh(l List) bool {
	meta := m.ReadMeta(l)
	if meta.FetchedAt.IsZero() {
		return false
	}
	if _, err := os.Stat(m.dataPath(l.Name)); err != nil {
		return false
	}
	at := meta.CheckedAt
	if at.IsZero() {
		at = meta.FetchedAt
	}
	return time.Since(at) < m.ttl()
}

// Update downloads l when it is stale (or force), using If-None-Match /
// If-Modified-Since, and returns its sidecar. A failed download keeps the
// previous copy and records the error.
func (m *Manager) Update(ctx context.Context, l List, force bool) (Meta, error) {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return Meta{}, err
	}
	meta := m.ReadMeta(l)
	if !force && m.Fresh(l) {
		return meta, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL, nil)
	if err != nil {
		return meta, err
	}
	req.Header.Set("User-Agent", "SilkRouter/4 (+https://github.com/Szmelc-INC/SilkRouter)")
	if _, statErr := os.Stat(m.dataPath(l.Name)); statErr == nil && !force {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}
	fail := func(err error) (Meta, error) {
		meta.Error, meta.CheckedAt = err.Error(), time.Now().UTC()
		_ = m.writeMeta(meta)
		return meta, err
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return fail(fmt.Errorf("%s: %w", l.Name, err))
	}
	defer resp.Body.Close()
	now := time.Now().UTC()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		meta.CheckedAt, meta.Error = now, ""
		return meta, m.writeMeta(meta)
	case resp.StatusCode != http.StatusOK:
		return fail(fmt.Errorf("%s: HTTP %s", l.Name, resp.Status))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return fail(fmt.Errorf("%s: %w", l.Name, err))
	}
	set, err := Parse(l, bytes.NewReader(data))
	if err != nil {
		return fail(fmt.Errorf("%s: %w", l.Name, err))
	}
	if set.Entries == 0 {
		return fail(fmt.Errorf("%s: no IP addresses in the download", l.Name))
	}
	if err := writeAtomic(m.dataPath(l.Name), data); err != nil {
		return fail(err)
	}
	meta = Meta{Name: l.Name, Kind: l.Kind, URL: l.URL, FetchedAt: now, CheckedAt: now, Entries: set.Entries,
		Bytes: int64(len(data)), ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	return meta, m.writeMeta(meta)
}

// LoadResult is one list of Load.
type LoadResult struct {
	Meta Meta
	Set  *Set // nil when the list is unavailable
	Err  error
}

// Load updates stale lists (in parallel, at most 4 downloads at a time)
// and parses every available local copy. update=false only parses what is
// on disk.
func (m *Manager) Load(ctx context.Context, update, force bool) []LoadResult {
	out := make([]LoadResult, len(m.Lists))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, l := range m.Lists {
		wg.Add(1)
		go func(i int, l List) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := LoadResult{Meta: m.ReadMeta(l)}
			if update {
				res.Meta, res.Err = m.Update(ctx, l, force)
			}
			f, err := os.Open(m.dataPath(l.Name))
			if err != nil {
				if res.Err == nil {
					res.Err = fmt.Errorf("%s: not downloaded yet (px lists update)", l.Name)
				}
				out[i] = res
				return
			}
			set, perr := Parse(l, f)
			f.Close()
			if perr != nil {
				res.Err = errors.Join(res.Err, perr)
			} else {
				set.FetchedAt = res.Meta.FetchedAt
				res.Set = set
			}
			out[i] = res
		}(i, l)
	}
	wg.Wait()
	return out
}

// Sets returns the loaded sets of results (skipping unavailable lists).
func Sets(results []LoadResult) []*Set {
	var out []*Set
	for _, r := range results {
		if r.Set != nil {
			out = append(out, r.Set)
		}
	}
	return out
}

// Match is one list verdict for an IP.
type Match struct {
	List   string
	Kind   string
	Listed bool
}

// MatchAll checks ip against every set.
func MatchAll(sets []*Set, ip netip.Addr) []Match {
	out := make([]Match, 0, len(sets))
	for _, s := range sets {
		out = append(out, Match{List: s.Name, Kind: s.Kind, Listed: s.Contains(ip)})
	}
	return out
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	_ = os.Chmod(tmp.Name(), 0o600)
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
