package relay

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
)

// upstream is a fake HTTP CONNECT proxy that tunnels to whatever target
// is asked (only local test servers here) and counts tunnels.
func upstream(t *testing.T, n *atomic.Int32) proxy.Proxy {
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
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				up, err := net.Dial("tcp", req.Host)
				if err != nil {
					fmt.Fprint(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				n.Add(1)
				fmt.Fprint(c, "HTTP/1.1 200 OK\r\n\r\n")
				go io.Copy(up, br)
				io.Copy(c, up)
			}(c)
		}
	}()
	p, _ := proxy.Parse("http://" + ln.Addr().String())
	return p
}

func deadUpstream(t *testing.T) proxy.Proxy {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	p, _ := proxy.Parse("socks5://" + addr)
	return p
}

type picker struct {
	mu    sync.Mutex
	list  []proxy.Proxy
	calls int
}

func (pk *picker) pick(_ context.Context, avoid []string) (proxy.Proxy, error) {
	pk.mu.Lock()
	defer pk.mu.Unlock()
	pk.calls++
	for _, p := range pk.list {
		if !contains(avoid, p.Key()) {
			return p, nil
		}
	}
	return pk.list[0], nil
}

func startRelay(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return s, ln.Addr().String()
}

func target(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// get fetches path from the target through a tunnel opened by p.
func get(t *testing.T, p proxy.Proxy, targetAddr, path string) string {
	t.Helper()
	c, err := p.Dial(context.Background(), targetAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial via %s: %v", p.Key(), err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, targetAddr)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestRelayProtocols(t *testing.T) {
	var tunnels atomic.Int32
	up := upstream(t, &tunnels)
	tg := target(t)
	tAddr := tg.Listener.Addr().String()
	var events []Event
	var emu sync.Mutex
	pk := &picker{list: []proxy.Proxy{up}}
	_, addr := startRelay(t, Options{Pick: pk.pick, OnEvent: func(e Event) { emu.Lock(); events = append(events, e); emu.Unlock() }})

	for _, scheme := range []string{"socks5", "socks4", "http"} {
		p, _ := proxy.Parse(scheme + "://" + addr)
		if body := get(t, p, tAddr, "/"+scheme); body != "hello /"+scheme {
			t.Fatalf("%s: %q", scheme, body)
		}
	}
	// socks4a: hostname resolved by the upstream side.
	_, port, _ := net.SplitHostPort(tAddr)
	p4a, _ := proxy.Parse("socks4a://" + addr)
	if body := get(t, p4a, net.JoinHostPort("localhost", port), "/4a"); body != "hello /4a" {
		t.Fatalf("socks4a: %q", body)
	}
	// Plain HTTP proxying (absolute-URI request) with net/http.
	u, _ := url.Parse("http://" + addr)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 5 * time.Second}
	resp, err := client.Get(tg.URL + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello /plain" {
		t.Fatalf("plain http: %q", b)
	}
	if tunnels.Load() != 5 {
		t.Fatalf("upstream tunnels: %d", tunnels.Load())
	}
	emu.Lock()
	kinds := map[string]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	emu.Unlock()
	if kinds[EventSwitch] != 1 || kinds[EventDialOK] != 5 {
		t.Fatalf("events: %v", kinds)
	}
	// Control API.
	var st Status
	if err := Control(context.Background(), addr, "status", &st); err != nil || st.Upstream != up.Key() || st.Conns < 5 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if err := Control(context.Background(), addr, "nope", nil); err == nil {
		t.Fatal("unknown endpoint accepted")
	}
}

func TestRelayFailoverAndRotation(t *testing.T) {
	var tunnels atomic.Int32
	good, dead := upstream(t, &tunnels), deadUpstream(t)
	tg := target(t)
	tAddr := tg.Listener.Addr().String()
	pk := &picker{list: []proxy.Proxy{dead, good}}
	var fails atomic.Int32
	srv, addr := startRelay(t, Options{Pick: pk.pick, MaxTries: 3, DialTimeout: 2 * time.Second, OnEvent: func(e Event) {
		if e.Kind == EventDialFail {
			fails.Add(1)
		}
	}})
	p, _ := proxy.Parse("socks5://" + addr)
	if body := get(t, p, tAddr, "/x"); body != "hello /x" {
		t.Fatalf("failover: %q", body)
	}
	if cur, _ := srv.Current(); cur.Key() != good.Key() || fails.Load() != 1 {
		t.Fatalf("current %s, fails %d", cur.Key(), fails.Load())
	}
	// Sticky: the good upstream stays; no new picks.
	calls := pk.calls
	get(t, p, tAddr, "/y")
	if pk.calls != calls {
		t.Fatal("sticky relay picked again")
	}
	// Manual rotation through the control API.
	pk.list = []proxy.Proxy{good, upstream(t, &tunnels)}
	if err := Control(context.Background(), addr, "rotate", nil); err != nil {
		t.Fatal(err)
	}
	if cur, _ := srv.Current(); cur.Key() == good.Key() {
		t.Fatal("rotate kept the upstream")
	}
	if st := srv.Status(); st.Switches != 3 {
		t.Fatalf("switches: %d", st.Switches)
	}

	// per-conn: a pick for every connection.
	pk2 := &picker{list: []proxy.Proxy{good}}
	_, addr2 := startRelay(t, Options{Pick: pk2.pick, Rotate: RotatePerConn})
	p2, _ := proxy.Parse("http://" + addr2)
	for i := 0; i < 3; i++ {
		get(t, p2, tAddr, "/")
	}
	if pk2.calls != 3 {
		t.Fatalf("per-conn picks: %d", pk2.calls)
	}
	// Every upstream dead: the client gets an error, not a hang.
	pk3 := &picker{list: []proxy.Proxy{dead}}
	_, addr3 := startRelay(t, Options{Pick: pk3.pick, MaxTries: 2, DialTimeout: time.Second})
	p3, _ := proxy.Parse("socks5://" + addr3)
	if _, err := p3.Dial(context.Background(), tAddr, 5*time.Second); err == nil {
		t.Fatal("dial through a dead upstream succeeded")
	}
}

func TestRelayAuthAndStop(t *testing.T) {
	var tunnels atomic.Int32
	up := upstream(t, &tunnels)
	tg := target(t)
	tAddr := tg.Listener.Addr().String()
	pk := &picker{list: []proxy.Proxy{up}}
	srv, addr := startRelay(t, Options{Pick: pk.pick, Auth: "u:secret"})
	noAuth, _ := proxy.Parse("socks5://" + addr)
	if _, err := noAuth.Dial(context.Background(), tAddr, 3*time.Second); err == nil {
		t.Fatal("socks5 without credentials accepted")
	}
	withAuth, _ := proxy.Parse("socks5://u:secret@" + addr)
	if body := get(t, withAuth, tAddr, "/a"); body != "hello /a" {
		t.Fatalf("socks5 auth: %q", body)
	}
	wrong, _ := proxy.Parse("http://u:bad@" + addr)
	if _, err := wrong.Dial(context.Background(), tAddr, 3*time.Second); err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("http wrong password: %v", err)
	}
	right, _ := proxy.Parse("http://u:secret@" + addr)
	if body := get(t, right, tAddr, "/b"); body != "hello /b" {
		t.Fatalf("http auth: %q", body)
	}
	s4, _ := proxy.Parse("socks4://" + addr)
	if _, err := s4.Dial(context.Background(), tAddr, 3*time.Second); err == nil {
		t.Fatal("socks4 accepted while auth is required")
	}
	// /stop ends Serve.
	if err := Control(context.Background(), addr, "stop", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("relay still listening after stop (status %+v)", srv.Status())
}
