package cache

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
)

// forwardProxy starts a minimal HTTP forward proxy and returns its address.
func forwardProxy(t *testing.T) string {
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
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				up, err := net.Dial("tcp", req.URL.Host)
				if err != nil {
					io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				req.Write(up)
				io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

func TestBuild(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 204, matches a status probe
	}))
	defer target.Close()

	pxAddr := forwardProxy(t)
	ph, pp := hostPort(t, pxAddr)

	// Three live proxies (all the same forward proxy) + two dead ones.
	live := proxy.Proxy{Scheme: proxy.HTTP, Host: ph, Port: pp}
	dead1 := proxy.Proxy{Scheme: proxy.HTTP, Host: "127.0.0.1", Port: 1}
	dead2 := proxy.Proxy{Scheme: proxy.SOCKS5, Host: "127.0.0.1", Port: 1}
	candidates := []proxy.Proxy{live, live, live, dead1, dead2}

	pr := probe.Custom(target.URL)
	res := Build(context.Background(), candidates, BuildOptions{
		Probe: pr, Jobs: 4, Tries: 1, Timeout: 2 * time.Second,
	}, io.Discard)

	if res.Tested != len(candidates) {
		t.Errorf("Tested = %d, want %d", res.Tested, len(candidates))
	}
	if len(res.Working) != 3 {
		t.Fatalf("Working = %d, want 3", len(res.Working))
	}
	for _, p := range res.Working {
		if p.LatencyMS <= 0 {
			t.Errorf("working proxy %s has no latency", p.URL())
		}
	}
}

func TestMergeAndSort(t *testing.T) {
	existing := []proxy.Proxy{
		{Scheme: proxy.HTTP, Host: "1.1.1.1", Port: 80, LatencyMS: 500},
		{Scheme: proxy.SOCKS5, Host: "2.2.2.2", Port: 1080, LatencyMS: 100},
	}
	fresh := []proxy.Proxy{
		{Scheme: proxy.HTTP, Host: "1.1.1.1", Port: 80, LatencyMS: 50}, // new timing wins
		{Scheme: proxy.HTTP, Host: "3.3.3.3", Port: 8080, LatencyMS: 300},
	}
	got := Merge(existing, fresh)
	if len(got) != 3 {
		t.Fatalf("Merge len = %d, want 3", len(got))
	}
	// Fastest first: 1.1.1.1 (50) < 2.2.2.2 (100) < 3.3.3.3 (300).
	if got[0].Host != "1.1.1.1" || got[0].LatencyMS != 50 {
		t.Errorf("head = %+v, want 1.1.1.1@50", got[0])
	}
	if got[2].Host != "3.3.3.3" {
		t.Errorf("tail = %+v, want 3.3.3.3", got[2])
	}
}
