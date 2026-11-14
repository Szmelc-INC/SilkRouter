package reputation

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseAndContains(t *testing.T) {
	src := `# comment
1.2.3.4
10.0.0.0/8 ; SBL123
10.1.0.0/16
192.168.1.0/24   extra columns
not an ip
2001:db8::/32
::ffff:8.8.8.8
`
	proxies := "198.51.100.10:8080\nsocks5://198.51.100.11:1080\nhttp://user:pw@198.51.100.12:3128\n[2001:db9::5]:1080\n"
	ps, err := Parse(List{Name: "p", Kind: KindProxyList}, strings.NewReader(proxies))
	if err != nil || ps.Entries != 4 {
		t.Fatalf("proxy list: %v %d", err, ps.Entries)
	}
	for _, ip := range []string{"198.51.100.10", "198.51.100.11", "198.51.100.12", "2001:db9::5"} {
		if !ps.Contains(netip.MustParseAddr(ip)) {
			t.Errorf("proxy list: %s missing", ip)
		}
	}
	s, err := Parse(List{Name: "t", Kind: KindIPSet}, strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if s.Entries != 6 {
		t.Fatalf("entries %d", s.Entries)
	}
	yes := []string{"1.2.3.4", "10.0.0.0", "10.255.255.255", "10.1.2.3", "192.168.1.200", "2001:db8::1", "8.8.8.8"}
	no := []string{"1.2.3.5", "11.0.0.0", "192.168.2.1", "2001:db9::1", "9.9.9.9"}
	for _, ip := range yes {
		if !s.Contains(netip.MustParseAddr(ip)) {
			t.Errorf("%s should be listed", ip)
		}
	}
	for _, ip := range no {
		if s.Contains(netip.MustParseAddr(ip)) {
			t.Errorf("%s should not be listed", ip)
		}
	}
	if len(s.v4) != 4 { // 10/8 swallows 10.1/16
		t.Fatalf("merged ranges: %+v", s.v4)
	}
	var nilSet *Set
	if nilSet.Contains(netip.MustParseAddr("1.2.3.4")) {
		t.Fatal("nil set")
	}
}

func TestSelectAndCustom(t *testing.T) {
	if got, err := Select(Builtin, nil); err != nil || len(got) != len(Builtin) {
		t.Fatal("select all")
	}
	if got, err := Select(Builtin, []string{"tor-exits"}); err != nil || len(got) != 1 || got[0].Kind != KindTorExit {
		t.Fatal("select one")
	}
	if _, err := Select(Builtin, []string{"nope"}); err == nil {
		t.Fatal("unknown list accepted")
	}
	l, err := ParseCustom("mine|proxylist|https://example.com/x.txt")
	if err != nil || l.Name != "mine" {
		t.Fatal(err)
	}
	for _, bad := range []string{"x", "a|weird|https://x", "|ipset|https://x", "a|ipset|ftp://x"} {
		if _, err := ParseCustom(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	names := map[string]bool{}
	for _, l := range Builtin {
		if names[l.Name] || l.URL == "" || l.Desc == "" {
			t.Errorf("builtin list %+v", l)
		}
		names[l.Name] = true
	}
}

func TestManagerUpdateCaching(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte("203.0.113.0/24\n198.51.100.7\n"))
	}))
	defer srv.Close()
	m := &Manager{Dir: t.TempDir(), TTL: time.Hour, Lists: []List{
		{Name: "a", Kind: KindProxyList, URL: srv.URL + "/a"},
		{Name: "gone", Kind: KindIPSet, URL: srv.URL + "/missing"},
	}}
	ctx := context.Background()
	res := m.Load(ctx, true, false)
	if res[0].Err != nil || res[0].Set == nil || res[0].Meta.Entries != 2 {
		t.Fatalf("a: %+v", res[0])
	}
	if res[1].Err == nil || res[1].Set != nil {
		t.Fatalf("missing list: %+v", res[1])
	}
	if !res[0].Set.Contains(netip.MustParseAddr("203.0.113.99")) {
		t.Fatal("contains")
	}
	n := hits.Load()
	// Fresh: no request.
	m.Load(ctx, true, false)
	if hits.Load() != n+1 { // only the failed list is retried
		t.Fatalf("fresh list re-downloaded: %d -> %d", n, hits.Load())
	}
	// Forced: conditional request is skipped, full download again.
	if meta, err := m.Update(ctx, m.Lists[0], true); err != nil || meta.Entries != 2 {
		t.Fatalf("force: %+v %v", meta, err)
	}
	// Stale: conditional request -> 304 keeps the copy.
	m.TTL = time.Nanosecond
	time.Sleep(time.Millisecond)
	meta, err := m.Update(ctx, m.Lists[0], false)
	if err != nil || meta.Entries != 2 || meta.Error != "" {
		t.Fatalf("304: %+v %v", meta, err)
	}
	st := m.Status()
	if len(st) != 2 || st[1].Error == "" {
		t.Fatalf("status: %+v", st)
	}
	// Offline load parses the local copy.
	if res := m.Load(ctx, false, false); res[0].Set == nil {
		t.Fatal("offline load")
	}
	sets := Sets(res)
	if len(sets) != 1 {
		t.Fatal("sets")
	}
	ms := MatchAll(sets, netip.MustParseAddr("198.51.100.7"))
	if len(ms) != 1 || !ms[0].Listed || ms[0].List != "a" {
		t.Fatalf("match: %+v", ms)
	}
}

func TestDNSBL(t *testing.T) {
	if got := ReverseName(netip.MustParseAddr("1.2.3.4"), "zen.example."); got != "4.3.2.1.zen.example" {
		t.Fatal(got)
	}
	if ReverseName(netip.MustParseAddr("2001:db8::1"), "z") != "" {
		t.Fatal("ipv6")
	}
	c := &DNSBL{
		Zones: []string{"listed.test", "clean.test", "refused.test", "broken.test"},
		Lookup: func(_ context.Context, host string) ([]string, error) {
			switch {
			case strings.HasSuffix(host, "listed.test"):
				return []string{"127.0.0.4", "127.0.0.2"}, nil
			case strings.HasSuffix(host, "refused.test"):
				return []string{"127.255.255.254"}, nil
			case strings.HasSuffix(host, "broken.test"):
				return nil, &net.DNSError{Err: "server misbehaving", Name: host}
			}
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		},
	}
	res := c.Check(context.Background(), netip.MustParseAddr("192.0.2.1"))
	if !res[0].Listed || strings.Join(res[0].Codes, ",") != "127.0.0.2,127.0.0.4" || res[0].Err != nil {
		t.Fatalf("listed: %+v", res[0])
	}
	if res[1].Listed || res[1].Err != nil {
		t.Fatalf("clean: %+v", res[1])
	}
	if res[2].Listed || res[2].Err == nil {
		t.Fatalf("refused: %+v", res[2])
	}
	if res[3].Listed || res[3].Err == nil {
		t.Fatalf("broken: %+v", res[3])
	}
}

func TestCache(t *testing.T) {
	var c Cache[int]
	var calls atomic.Int32
	f := func() int { calls.Add(1); return 42 }
	v, hit := c.Get("k", f)
	if v != 42 || hit {
		t.Fatal("first")
	}
	v, hit = c.Get("k", f)
	if v != 42 || !hit || calls.Load() != 1 {
		t.Fatal("second")
	}
}

func TestGeoClient(t *testing.T) {
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		if r.Method != http.MethodPost || !strings.Contains(r.URL.RawQuery, "fields=") {
			http.Error(w, "bad", 400)
			return
		}
		var ips []string
		json.NewDecoder(r.Body).Decode(&ips)
		var out []Geo
		for _, ip := range ips {
			g := Geo{IP: ip, Status: "success", CountryCode: "PL", AS: "AS5617 Orange Polska", ISP: "Orange", Hosting: ip == "192.0.2.2"}
			if ip == "192.0.2.3" {
				g = Geo{IP: ip, Status: "fail", Message: "reserved range"}
			}
			out = append(out, g)
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	c := &GeoClient{URL: srv.URL, Interval: time.Millisecond}
	ips := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}
	for i := 0; i < 150; i++ {
		ips = append(ips, "198.51.100."+strings.Repeat("1", 1))
	}
	res, err := c.Lookup(context.Background(), ips)
	if err != nil {
		t.Fatal(err)
	}
	if reqs.Load() != 2 {
		t.Fatalf("requests: %d (want 100 + 53 split in 2)", reqs.Load())
	}
	g := res["192.0.2.2"]
	if !g.Hosting || g.ASN() != "AS5617" || g.Organization() != "Orange" || g.CountryCode != "PL" {
		t.Fatalf("%+v", g)
	}
	if _, ok := res["192.0.2.3"]; ok {
		t.Fatal("failed lookup kept")
	}
}
