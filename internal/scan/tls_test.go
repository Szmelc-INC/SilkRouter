package scan

import (
	"bufio"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

// tunnelProxy is an HTTP CONNECT proxy that pipes every tunnel to target
// (a local TLS server), whatever host the client asked for.
func tunnelProxy(t *testing.T, target string, connects *atomic.Int32) string {
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
				connects.Add(1)
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				fmt.Fprint(c, "HTTP/1.1 200 OK\r\n\r\n")
				go io.Copy(up, br)
				io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestHTTPSCheckVerifiesCertificates(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fl=1\nip=203.0.113.5\nloc=DE\ntls=TLSv1.3\n")
	}))
	defer srv.Close()
	var connects atomic.Int32
	px := mustProxy(t, "http://"+tunnelProxy(t, srv.Listener.Addr().String(), &connects))
	trusted := x509.NewCertPool()
	trusted.AddCert(srv.Certificate())
	ctx := context.Background()

	// httptest's certificate is valid for example.com: trusted roots pass.
	res := TLSFetch(ctx, px, "https://example.com/cdn-cgi/trace", 5*time.Second, trusted)
	if !res.OK || res.MITM || res.Verified == nil || !*res.Verified || !strings.Contains(res.Body, "loc=DE") {
		t.Fatalf("trusted: %+v", res)
	}
	// Wrong host for the certificate: interception.
	res = TLSFetch(ctx, px, "https://www.cloudflare.com/", 5*time.Second, trusted)
	if res.OK || !res.MITM || res.ErrClass != db.ErrTLS || res.Issuer == "" {
		t.Fatalf("host mismatch: %+v", res)
	}

	// Through the engine: untrusted roots = MITM, recorded once, no fallback.
	run := func(roots *x509.CertPool, urls ...string) (*memSink, Report) {
		sink := &memSink{}
		var rep Report
		eng := New(Options{Mode: ModeFull, Workers: 1, Tries: 1, Timeout: 5 * time.Second, HTTPSURLs: urls, TLSRoots: roots}, sink)
		r := Report{}
		eng.https(ctx, Target{ID: 1, Proxy: px}, &r)
		rep = r
		return sink, rep
	}
	connects.Store(0)
	sink, rep := run(x509.NewCertPool(), "https://example.com/a", "https://example.com/b")
	if rep.HTTPS == nil || *rep.HTTPS || rep.MITM == nil || !*rep.MITM || rep.TLSIssuer == "" {
		t.Fatalf("mitm report: %+v", rep)
	}
	if len(sink.ms) != 1 || connects.Load() != 1 || !strings.Contains(sink.ms[0].Detail, `"mitm":true`) || sink.ms[0].Probe != ProbeHTTPS {
		t.Fatalf("mitm facts: %d facts, %d tunnels, %+v", len(sink.ms), connects.Load(), sink.ms)
	}
	// A non-TLS failure (bad status) falls back to the next URL.
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blocked" {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv2.Close()
	px = mustProxy(t, "http://"+tunnelProxy(t, srv2.Listener.Addr().String(), &connects))
	roots2 := x509.NewCertPool()
	roots2.AddCert(srv2.Certificate())
	sink, rep = run(roots2, "https://example.com/blocked", "https://example.com/fine")
	if rep.HTTPS == nil || !*rep.HTTPS || rep.MITM == nil || *rep.MITM || len(sink.ms) != 2 ||
		sink.ms[0].OK || sink.ms[0].ErrClass != db.ErrHTTPStatus || !sink.ms[1].OK {
		t.Fatalf("fallback: %+v %+v", rep, sink.ms)
	}
	if res := TLSFetch(ctx, px, "http://example.com/", time.Second, nil); res.OK || res.ErrClass != db.ErrOther {
		t.Fatalf("non-https URL: %+v", res)
	}
}
