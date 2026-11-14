package scan

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/reputation"
)

// fakeProxy is a tiny HTTP proxy that answers every request itself: 204 for
// /generate_204, an IP for the ipify probe, a judge JSON for /get, and a
// working CONNECT tunnel to a fake HTTP server. It never touches the
// network.
func fakeProxy(t *testing.T, judgeHeaders map[string]string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFake(c, judgeHeaders)
		}
	}()
	return ln.Addr().String()
}

func serveFake(c net.Conn, judge map[string]string) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	if b, err := br.Peek(1); err != nil || b[0] == 4 || b[0] == 5 {
		return // not a SOCKS server
	}
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method == http.MethodConnect {
		fmt.Fprint(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		inner, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		answer(c, inner, judge)
		return
	}
	answer(c, req, judge)
}

func answer(c net.Conn, r *http.Request, judge map[string]string) {
	write := func(status int, body string) {
		fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, http.StatusText(status), len(body), body)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/generate_204"):
		write(204, "")
	case r.Host == "api.ipify.org":
		write(200, "203.0.113.77")
	case r.URL.Path == "/get":
		js, _ := json.Marshal(map[string]any{"headers": judge, "origin": "203.0.113.77"})
		write(200, string(js))
	default:
		write(200, "Example Domain")
	}
}

type memSink struct {
	mu sync.Mutex
	ms []db.Measurement
	rs []db.Reputation
	is []db.ProxyInfo
}

func (s *memSink) Measure(m db.Measurement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ms = append(s.ms, m)
	return nil
}
func (s *memSink) Reputation(r db.Reputation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rs = append(s.rs, r)
	return nil
}
func (s *memSink) Info(i db.ProxyInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.is = append(s.is, i)
	return nil
}

func mustProxy(t *testing.T, s string) proxy.Proxy {
	t.Helper()
	p, err := proxy.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFullScan(t *testing.T) {
	alive := fakeProxy(t, map[string]string{"Via": "1.1 squid"})
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := closed.Addr().String()
	closed.Close()

	list, _ := reputation.Parse(reputation.List{Name: "known", Kind: reputation.KindProxyList},
		strings.NewReader("203.0.113.0/24\n"))
	geoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ips []string
		json.NewDecoder(r.Body).Decode(&ips)
		var out []reputation.Geo
		for _, ip := range ips {
			out = append(out, reputation.Geo{IP: ip, Status: "success", CountryCode: "PL", AS: "AS1 Test", Hosting: true})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer geoSrv.Close()

	probes := []probe.Probe{}
	for _, n := range []string{"cloudflare", "google"} {
		p, _ := probe.Get(n)
		probes = append(probes, p)
	}
	exit, _ := probe.Get("ipify")
	sink := &memSink{}
	var reports []Report
	var rmu sync.Mutex
	eng := New(Options{
		Mode: ModeFull, Workers: 2, Tries: 2, Timeout: 2 * time.Second, Probes: probes, FailFast: 2,
		ExitProbe: &exit, JudgeURL: "http://judge.test/get", RealIP: "198.51.100.200", Protocols: true,
		Lists: []*reputation.Set{list},
		DNSBL: &reputation.DNSBL{Zones: []string{"bl.test"}, Lookup: func(_ context.Context, host string) ([]string, error) {
			if strings.HasPrefix(host, "77.113.0.203.") {
				return []string{"127.0.0.2"}, nil
			}
			return nil, &net.DNSError{Err: "nx", IsNotFound: true}
		}},
		Geo: &reputation.GeoClient{URL: geoSrv.URL, Interval: time.Millisecond},
		OnDone: func(r Report) {
			rmu.Lock()
			reports = append(reports, r)
			rmu.Unlock()
		},
	}, sink)
	targets := []Target{
		{ID: 1, Proxy: mustProxy(t, "http://"+alive)},
		{ID: 2, Proxy: mustProxy(t, "http://"+deadAddr)},
	}
	if err := eng.Run(context.Background(), targets); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports: %+v", reports)
	}
	byID := map[int64]Report{}
	for _, r := range reports {
		byID[r.ID] = r
	}
	a, d := byID[1], byID[2]
	if !a.Alive || a.ProbesOK != 2 || a.MS <= 0 || a.ExitIP != "203.0.113.77" {
		t.Fatalf("alive report: %+v", a)
	}
	if a.Anonymity != "anonymous" {
		t.Fatalf("Via header should grade anonymous: %q", a.Anonymity)
	}
	if strings.Join(a.Protocols, ",") != "http" {
		t.Fatalf("protocols: %v", a.Protocols)
	}
	if a.Listed != 2 || strings.Join(a.ListedOn, ",") != "bl.test,known" {
		t.Fatalf("listed: %d %v", a.Listed, a.ListedOn)
	}
	if d.Alive || d.ErrClass == "" {
		t.Fatalf("dead report: %+v", d)
	}
	// Facts: tcp failure for the dead one; probes, exit, judge for the alive.
	kinds := map[string]int{}
	for _, m := range sink.ms {
		if m.Kind != db.KindScan {
			t.Fatalf("kind %q", m.Kind)
		}
		kinds[fmt.Sprintf("%d/%s/%v", m.ProxyID, m.Probe, m.OK)]++
	}
	for _, want := range []string{"1/cloudflare/true", "1/google/true", "1/exit-ip/true", "1/judge/true", "2/tcp/false"} {
		if kinds[want] != 1 {
			t.Errorf("missing fact %s: %v", want, kinds)
		}
	}
	var gotGeo, gotFull bool
	for _, i := range sink.is {
		if i.ProxyID == 1 && i.Country == "PL" && i.Hosting != nil && *i.Hosting {
			gotGeo = true
		}
		if i.ProxyID == 1 && !i.FullScanAt.IsZero() && i.Anonymity == "anonymous" && i.JitterMS != nil {
			gotFull = true
		}
	}
	if !gotGeo || !gotFull {
		t.Fatalf("infos: %+v", sink.is)
	}
	var hosting bool
	for _, r := range sink.rs {
		if r.ListName == "ip-api:hosting" && r.Listed {
			hosting = true
		}
	}
	if !hosting {
		t.Fatalf("geo verdicts: %+v", sink.rs)
	}
	st := eng.Stats()
	if st.Done != 2 || st.Alive != 1 || st.Dead != 1 || st.Listed != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestQuickScanAndCancel(t *testing.T) {
	alive := fakeProxy(t, nil)
	cf, _ := probe.Get("cloudflare")
	sink := &memSink{}
	eng := New(Options{Mode: ModeQuick, Workers: 4, Tries: 1, Timeout: 2 * time.Second, Probes: []probe.Probe{cf}}, sink)
	var targets []Target
	for i := 1; i <= 5; i++ {
		targets = append(targets, Target{ID: int64(i), Proxy: mustProxy(t, "http://"+alive)})
	}
	if err := eng.Run(context.Background(), targets); err != nil {
		t.Fatal(err)
	}
	if len(sink.ms) != 5 || eng.Stats().Alive != 5 {
		t.Fatalf("quick: %d facts, %+v", len(sink.ms), eng.Stats())
	}
	// A cancelled scan stops feeding and returns the context error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eng = New(Options{Mode: ModeQuick, Workers: 1, Probes: []probe.Probe{cf}}, &memSink{})
	if err := eng.Run(ctx, targets); err == nil {
		t.Fatal("cancelled run returned nil")
	}
}

func TestGrade(t *testing.T) {
	cases := []struct {
		body, real, want string
	}{
		{`{"headers":{"Host":"x"},"origin":"203.0.113.1"}`, "198.51.100.1", "elite"},
		{`{"headers":{"Via":"1.1 squid"},"origin":"203.0.113.1"}`, "198.51.100.1", "anonymous"},
		{`{"headers":{},"origin":"203.0.113.1, 203.0.113.2"}`, "198.51.100.1", "anonymous"},
		{`{"headers":{"X-Forwarded-For":"198.51.100.1"},"origin":"198.51.100.1, 203.0.113.1"}`, "198.51.100.1", "transparent"},
		{"REMOTE_ADDR = 203.0.113.1\nHTTP_VIA = 1.1 proxy\n", "", "anonymous"},
		{"REMOTE_ADDR = 203.0.113.1\n", "", "elite"},
	}
	for _, c := range cases {
		if got, _ := Grade(c.body, c.real); got != c.want {
			t.Errorf("%s: %s want %s", c.body, got, c.want)
		}
	}
}

func TestHelpers(t *testing.T) {
	if median([]int{5, 1, 3}) != 3 || median([]int{1, 2, 3, 4}) != 3 || median(nil) != 0 {
		t.Fatal("median")
	}
	if stddev([]int{10, 10}) != 0 || stddev([]int{10, 20, 30}) != 10 {
		t.Fatalf("stddev %v", stddev([]int{10, 20, 30}))
	}
	kv := parseTrace("fl=1\nip=203.0.113.5\nloc=DE\n")
	if kv["ip"] != "203.0.113.5" || kv["loc"] != "DE" {
		t.Fatal(kv)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "  198.51.100.9\n") }))
	defer srv.Close()
	if ip, err := RealIP(context.Background(), srv.URL, time.Second); err != nil || ip != "198.51.100.9" {
		t.Fatalf("%q %v", ip, err)
	}
}
