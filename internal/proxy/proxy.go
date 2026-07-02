// Package proxy models a single proxy endpoint and knows how to dial through
// it. Everything here is pure standard library so the final binary stays
// dependency-free and cross-platform.
package proxy

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Scheme is a supported proxy protocol.
type Scheme string

const (
	HTTP    Scheme = "http"
	HTTPS   Scheme = "https"
	SOCKS4  Scheme = "socks4"
	SOCKS4A Scheme = "socks4a"
	SOCKS5  Scheme = "socks5"
)

// AllSchemes lists every protocol SilkRouter understands, in report order.
var AllSchemes = []Scheme{HTTP, HTTPS, SOCKS4, SOCKS4A, SOCKS5}

// IsSocks reports whether the scheme is a SOCKS variant.
func (s Scheme) IsSocks() bool {
	return s == SOCKS4 || s == SOCKS4A || s == SOCKS5
}

// Proxy is a single, fully-parsed proxy endpoint.
type Proxy struct {
	Scheme    Scheme
	Host      string // IP or hostname
	Port      int
	User      string // optional auth
	Pass      string
	LatencyMS int // measured round-trip; 0 when unknown
}

// Addr returns the host:port pair.
func (p Proxy) Addr() string {
	return net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

// URL returns scheme://[user:pass@]host:port.
func (p Proxy) URL() string {
	if p.User != "" {
		return fmt.Sprintf("%s://%s:%s@%s", p.Scheme, p.User, p.Pass, p.Addr())
	}
	return fmt.Sprintf("%s://%s", p.Scheme, p.Addr())
}

// String renders the canonical cache line: scheme://host:port [Nms].
func (p Proxy) String() string {
	if p.LatencyMS > 0 {
		return fmt.Sprintf("%s [%dms]", p.URL(), p.LatencyMS)
	}
	return p.URL()
}

// Key is a stable identity used for de-duplication (ignores latency/auth).
func (p Proxy) Key() string {
	return fmt.Sprintf("%s://%s", p.Scheme, p.Addr())
}

// URLValue builds a *url.URL, useful for http.ProxyURL.
func (p Proxy) URLValue() *url.URL {
	u := &url.URL{Scheme: string(p.Scheme), Host: p.Addr()}
	if p.User != "" {
		u.User = url.UserPassword(p.User, p.Pass)
	}
	return u
}

// latencyRe matches the trailing "[123ms]" latency annotation, case-insensitive.
var latencyRe = regexp.MustCompile(`(?i)\[\s*([0-9]+)\s*ms\s*\]`)

// Parse turns one list line into a Proxy. It accepts every format the original
// bash tooling emitted or consumed:
//
//	scheme://ip:port
//	scheme://user:pass@ip:port
//	scheme://ip:port [123ms]
//	ip:port          (assumes http)
//	ip               (assumes http on port 80)
//
// Comments (after '#') and surrounding whitespace are stripped. Blank/comment
// lines return an error the caller can treat as "skip".
func Parse(line string) (Proxy, error) {
	var p Proxy

	// Drop trailing comments and the [Nms] latency tag (capturing it first).
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	if m := latencyRe.FindStringSubmatch(line); m != nil {
		if ms, err := strconv.Atoi(m[1]); err == nil {
			p.LatencyMS = ms
		}
		line = latencyRe.ReplaceAllString(line, "")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return p, fmt.Errorf("empty line")
	}

	// Split off the scheme if present; otherwise default to http.
	scheme := HTTP
	rest := line
	if i := strings.Index(line, "://"); i >= 0 {
		s, err := ParseScheme(line[:i])
		if err != nil {
			return p, err
		}
		scheme = s
		rest = line[i+3:]
	}
	p.Scheme = scheme

	// Optional user:pass@ credentials.
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		cred := rest[:at]
		rest = rest[at+1:]
		if c := strings.IndexByte(cred, ':'); c >= 0 {
			p.User, p.Pass = cred[:c], cred[c+1:]
		} else {
			p.User = cred
		}
	}

	host, portStr, err := net.SplitHostPort(rest)
	if err != nil {
		// A bare token with no port is only accepted when it is a valid IPv4
		// (the historical "ip -> http:80" shorthand). Anything else — prose,
		// junk, hostnames without a port — is rejected so list parsing does
		// not silently swallow garbage lines.
		if net.ParseIP(rest) == nil || strings.Contains(rest, ":") {
			return p, fmt.Errorf("invalid host:port %q", rest)
		}
		host, portStr = rest, "80"
	}
	if !validHost(host) {
		return p, fmt.Errorf("invalid host %q", host)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return p, fmt.Errorf("invalid port %q", portStr)
	}
	p.Host, p.Port = host, port
	return p, nil
}

// hostRe matches an IP or a plausible DNS hostname (no spaces/control chars).
var hostRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// validHost reports whether host is a usable IP or hostname.
func validHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return hostRe.MatchString(host)
}

// ParseScheme validates and normalises a protocol string.
func ParseScheme(s string) (Scheme, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "http":
		return HTTP, nil
	case "https":
		return HTTPS, nil
	case "socks4":
		return SOCKS4, nil
	case "socks4a":
		return SOCKS4A, nil
	case "socks5", "socks", "socks5h":
		return SOCKS5, nil
	default:
		return "", fmt.Errorf("unsupported scheme %q", s)
	}
}
