package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		wantErr  bool
		scheme   Scheme
		host     string
		port     int
		latency  int
		user     string
		password string
	}{
		{in: "socks5://1.2.3.4:1080", scheme: SOCKS5, host: "1.2.3.4", port: 1080},
		{in: "http://5.6.7.8:3128 [120ms]", scheme: HTTP, host: "5.6.7.8", port: 3128, latency: 120},
		{in: "9.10.11.12:4145", scheme: HTTP, host: "9.10.11.12", port: 4145},
		{in: "8.8.8.8", scheme: HTTP, host: "8.8.8.8", port: 80},
		{in: "socks4a://proxy.example.com:1080", scheme: SOCKS4A, host: "proxy.example.com", port: 1080},
		{in: "http://user:pass@1.2.3.4:8080", scheme: HTTP, host: "1.2.3.4", port: 8080, user: "user", password: "pass"},
		{in: "# comment", wantErr: true},
		{in: "", wantErr: true},
		{in: "garbage line that should be skipped", wantErr: true},
		{in: "ftp://1.2.3.4:21", wantErr: true},
		{in: "1.2.3.4:99999", wantErr: true},
		{in: "hello", wantErr: true}, // bare non-IP token
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) = %+v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got.Scheme != c.scheme || got.Host != c.host || got.Port != c.port ||
			got.LatencyMS != c.latency || got.User != c.user || got.Pass != c.password {
			t.Errorf("Parse(%q) = %+v, want scheme=%s host=%s port=%d lat=%d user=%s",
				c.in, got, c.scheme, c.host, c.port, c.latency, c.user)
		}
	}
}

// --- integration tests exercising the real handshakes via mock proxies -----

// targetServer returns an HTTP server that always replies "hello".
func targetServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "hello")
	}))
}

func mustGet(t *testing.T, p Proxy, url string) string {
	t.Helper()
	resp, err := p.Client(5 * time.Second).Get(url)
	if err != nil {
		t.Fatalf("GET through %s: %v", p.URL(), err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func proxyFromListener(scheme Scheme, ln net.Listener) Proxy {
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return Proxy{Scheme: scheme, Host: host, Port: port}
}

func pipe(a, b net.Conn) {
	go func() { io.Copy(a, b); a.Close() }()
	io.Copy(b, a)
	b.Close()
}

func TestSocks5Transport(t *testing.T) {
	target := targetServer(t)
	defer target.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveSocks5(ln)

	p := proxyFromListener(SOCKS5, ln)
	if got := mustGet(t, p, target.URL); got != "hello" {
		t.Errorf("socks5 body = %q, want hello", got)
	}
}

func TestSocks4Transport(t *testing.T) {
	target := targetServer(t)
	defer target.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveSocks4(ln)

	p := proxyFromListener(SOCKS4, ln)
	if got := mustGet(t, p, target.URL); got != "hello" {
		t.Errorf("socks4 body = %q, want hello", got)
	}
}

func TestHTTPProxyPlainGET(t *testing.T) {
	target := targetServer(t)
	defer target.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveHTTPProxy(ln)

	p := proxyFromListener(HTTP, ln)
	if got := mustGet(t, p, target.URL); got != "hello" {
		t.Errorf("http proxy body = %q, want hello", got)
	}
}

func TestHTTPConnectDial(t *testing.T) {
	// A raw TCP echo target reachable only via CONNECT.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveHTTPProxy(ln)

	p := proxyFromListener(HTTP, ln)
	conn, err := p.Dial(context.Background(), echoLn.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("CONNECT dial: %v", err)
	}
	defer conn.Close()
	io.WriteString(conn, "ping\n")
	got, _ := bufio.NewReader(conn).ReadString('\n')
	if strings.TrimSpace(got) != "ping" {
		t.Errorf("echo via CONNECT = %q, want ping", got)
	}
}

// --- minimal mock proxy servers --------------------------------------------

func serveSocks5(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleSocks5(c)
	}
}

func handleSocks5(c net.Conn) {
	br := bufio.NewReader(c)
	// greeting
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		c.Close()
		return
	}
	io.ReadFull(br, make([]byte, int(hdr[1]))) // methods
	c.Write([]byte{0x05, 0x00})                // no-auth
	// request
	rhdr := make([]byte, 4)
	if _, err := io.ReadFull(br, rhdr); err != nil {
		c.Close()
		return
	}
	var host string
	switch rhdr[3] {
	case 0x01:
		ip := make([]byte, 4)
		io.ReadFull(br, ip)
		host = net.IP(ip).String()
	case 0x03:
		l := make([]byte, 1)
		io.ReadFull(br, l)
		name := make([]byte, int(l[0]))
		io.ReadFull(br, name)
		host = string(name)
	}
	portB := make([]byte, 2)
	io.ReadFull(br, portB)
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portB))))
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		c.Close()
		return
	}
	// br may hold buffered bytes; flush them first.
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		upstream.Write(b)
	}
	pipe(c, upstream)
}

func serveSocks4(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleSocks4(c)
	}
}

func handleSocks4(c net.Conn) {
	br := bufio.NewReader(c)
	hdr := make([]byte, 8) // ver cmd port(2) ip(4)
	if _, err := io.ReadFull(br, hdr); err != nil {
		c.Close()
		return
	}
	// consume null-terminated userid
	for {
		b, err := br.ReadByte()
		if err != nil || b == 0 {
			break
		}
	}
	port := binary.BigEndian.Uint16(hdr[2:4])
	host := net.IP(hdr[4:8]).String()
	c.Write([]byte{0x00, 0x5a, 0, 0, 0, 0, 0, 0})
	upstream, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		c.Close()
		return
	}
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		upstream.Write(b)
	}
	pipe(c, upstream)
}

func serveHTTPProxy(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleHTTPProxy(c)
	}
}

func handleHTTPProxy(c net.Conn) {
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		c.Close()
		return
	}
	if req.Method == http.MethodConnect {
		upstream, err := net.Dial("tcp", req.Host)
		if err != nil {
			io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			c.Close()
			return
		}
		io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		pipe(c, upstream)
		return
	}
	// Absolute-form forward GET: dial origin and relay the request/response.
	upstream, err := net.Dial("tcp", req.URL.Host)
	if err != nil {
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		c.Close()
		return
	}
	req.Write(upstream)
	io.Copy(c, upstream)
	upstream.Close()
	c.Close()
}
