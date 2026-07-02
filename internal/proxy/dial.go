package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// DefaultTimeout is used when a caller passes a non-positive timeout.
const DefaultTimeout = 15 * time.Second

// Dial opens a TCP tunnel to target ("host:port") through this proxy and
// returns the tunnelled connection. It works for every supported scheme:
// SOCKS4/4a/5 use their native handshakes; HTTP/HTTPS proxies use CONNECT.
//
// This is the single choke point for "route arbitrary TCP through a proxy",
// which keeps adding new features (port checks, custom probes, ...) simple.
func (p Proxy) Dial(ctx context.Context, target string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	d := &net.Dialer{Timeout: timeout, KeepAlive: -1}

	conn, err := d.DialContext(ctx, "tcp", p.Addr())
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", p.Addr(), err)
	}
	// Bound the handshake with the deadline too, then clear it for the caller.
	_ = conn.SetDeadline(time.Now().Add(timeout))

	if err := p.handshake(ctx, conn, target); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// handshake negotiates the tunnel to target for the proxy's scheme.
func (p Proxy) handshake(ctx context.Context, conn net.Conn, target string) error {
	switch p.Scheme {
	case SOCKS5:
		return p.socks5Handshake(conn, target)
	case SOCKS4, SOCKS4A:
		return p.socks4Handshake(ctx, conn, target)
	case HTTP, HTTPS:
		return p.httpConnect(conn, target)
	default:
		return fmt.Errorf("unsupported scheme %q", p.Scheme)
	}
}

// httpConnect issues an HTTP CONNECT to tunnel raw TCP through an HTTP proxy.
func (p Proxy) httpConnect(conn net.Conn, target string) error {
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: make(http.Header),
	}
	if p.User != "" {
		req.SetBasicAuth(p.User, p.Pass)
	}
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("http connect write: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fmt.Errorf("http connect reply: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http connect failed: %s", resp.Status)
	}
	return nil
}

// Transport builds an *http.Transport that routes every request through this
// proxy. HTTP/HTTPS proxies use the native http.ProxyURL path (so plain and
// tunnelled requests both work); SOCKS proxies dial through Dial.
func (p Proxy) Transport(timeout time.Duration) *http.Transport {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	t := &http.Transport{
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: timeout,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	switch p.Scheme {
	case HTTP, HTTPS:
		t.Proxy = http.ProxyURL(p.URLValue())
		t.DialContext = (&net.Dialer{Timeout: timeout, KeepAlive: -1}).DialContext
	default: // SOCKS variants
		t.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
			return p.Dial(ctx, addr, timeout)
		}
	}
	return t
}

// Client returns an *http.Client bound to this proxy with an overall timeout.
func (p Proxy) Client(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Transport: p.Transport(timeout),
		Timeout:   timeout,
		// Do not follow redirects automatically for probes; callers decide.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
