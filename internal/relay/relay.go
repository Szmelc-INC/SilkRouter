// Package relay is SilkRouter's local proxy: one loopback port that speaks
// SOCKS5, SOCKS4/4a and HTTP (CONNECT and plain requests) and forwards
// every connection through an upstream proxy picked from the database, with
// failover (a dead upstream is replaced and the connection retried) and
// rotation (sticky, per connection, or every interval). Programs point at a
// stable 127.0.0.1 address while the upstream changes underneath.
//
// Control requests ("http://silkrouter.relay/status", "/rotate", "/stop")
// are answered on the same port, from loopback clients only.
package relay

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
)

// ControlHost is the magic host of the control API.
const ControlHost = "silkrouter.relay"

// Rotation modes.
const (
	RotateSticky   = "sticky"   // keep the upstream until it fails
	RotatePerConn  = "per-conn" // a new upstream for every client connection
	RotateInterval = "interval" // switch every Interval (and on failure)
)

// RotateModes lists the rotation modes.
var RotateModes = []string{RotateSticky, RotatePerConn, RotateInterval}

// Event kinds reported to Options.OnEvent.
const (
	EventDialOK   = "dial-ok"   // a tunnel through the upstream was opened
	EventDialFail = "dial-fail" // the upstream failed to open a tunnel
	EventSwitch   = "switch"    // the current upstream changed
)

// Event is something that happened to an upstream.
type Event struct {
	Kind     string
	Proxy    proxy.Proxy
	Previous proxy.Proxy // EventSwitch: the upstream replaced
	Target   string
	MS       int
	Err      error
	Reason   string // EventSwitch: why
}

// Options configure a Server.
type Options struct {
	// Pick returns an upstream, avoiding the given keys (scheme://host:port)
	// when it can. It must be safe for concurrent use.
	Pick        func(ctx context.Context, avoid []string) (proxy.Proxy, error)
	Rotate      string
	Interval    time.Duration // RotateInterval period
	MaxTries    int           // upstreams tried per client connection (default 3)
	DialTimeout time.Duration // tunnel setup through one upstream (default 10s)
	// Auth, when "user:pass", is required from SOCKS5 (RFC 1929) and HTTP
	// (Proxy-Authorization: Basic) clients; SOCKS4 clients are refused.
	Auth string
	// OnEvent receives upstream events (from many goroutines).
	OnEvent func(Event)
}

// Status is the relay's live state (GET /status).
type Status struct {
	Listen    string    `json:"listen"`
	Upstream  string    `json:"upstream,omitempty"`
	Since     time.Time `json:"since,omitzero"` // current upstream since
	Started   time.Time `json:"started"`
	Rotate    string    `json:"rotate"`
	Conns     int64     `json:"conns"`
	Active    int64     `json:"active"`
	Fails     int64     `json:"fails"`
	Switches  int64     `json:"switches"`
	BytesUp   int64     `json:"bytes_up"`
	BytesDown int64     `json:"bytes_down"`
	PID       int       `json:"pid,omitempty"`
}

// Server is one relay.
type Server struct {
	opts    Options
	ln      net.Listener
	started time.Time
	cancel  context.CancelFunc

	mu      sync.Mutex
	current *proxy.Proxy
	since   time.Time

	conns, active, fails, switches, up, down atomic.Int64
}

// New returns a server (call Serve).
func New(opts Options) *Server {
	if opts.MaxTries <= 0 {
		opts.MaxTries = 3
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.Rotate == "" {
		opts.Rotate = RotateSticky
	}
	return &Server{opts: opts}
}

// Addr is the listening address (after Serve started).
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Serve accepts connections on ln until ctx is cancelled or Stop is called.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel, s.ln, s.started = cancel, ln, time.Now().UTC()
	defer cancel()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	if s.opts.Rotate == RotateInterval && s.opts.Interval > 0 {
		go func() {
			t := time.NewTicker(s.opts.Interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_, _ = s.Rotate(ctx, "interval")
				}
			}
		}()
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		s.conns.Add(1)
		s.active.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.active.Add(-1)
			defer c.Close()
			s.handle(ctx, c)
		}()
	}
}

// Stop stops Serve.
func (s *Server) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Status returns the live counters.
func (s *Server) Status() Status {
	st := Status{Started: s.started, Rotate: s.opts.Rotate, Conns: s.conns.Load(), Active: s.active.Load(),
		Fails: s.fails.Load(), Switches: s.switches.Load(), BytesUp: s.up.Load(), BytesDown: s.down.Load()}
	if s.ln != nil {
		st.Listen = s.ln.Addr().String()
	}
	s.mu.Lock()
	if s.current != nil {
		st.Upstream, st.Since = s.current.Key(), s.since
	}
	s.mu.Unlock()
	return st
}

// Current returns the current upstream (ok = false before the first one).
func (s *Server) Current() (proxy.Proxy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return proxy.Proxy{}, false
	}
	return *s.current, true
}

// Rotate switches to another upstream now.
func (s *Server) Rotate(ctx context.Context, reason string) (proxy.Proxy, error) {
	s.mu.Lock()
	var avoid []string
	if s.current != nil {
		avoid = []string{s.current.Key()}
	}
	s.mu.Unlock()
	p, err := s.opts.Pick(ctx, avoid)
	if err != nil {
		return proxy.Proxy{}, err
	}
	s.setCurrent(p, reason)
	return p, nil
}

func (s *Server) setCurrent(p proxy.Proxy, reason string) {
	s.mu.Lock()
	prev := s.current
	if prev != nil && prev.Key() == p.Key() {
		s.mu.Unlock()
		return
	}
	cp := p
	s.current, s.since = &cp, time.Now().UTC()
	s.mu.Unlock()
	s.switches.Add(1)
	ev := Event{Kind: EventSwitch, Proxy: p, Reason: reason}
	if prev != nil {
		ev.Previous = *prev
	}
	s.emit(ev)
}

func (s *Server) emit(e Event) {
	if s.opts.OnEvent != nil {
		s.opts.OnEvent(e)
	}
}

// dial opens a tunnel to target through an upstream, failing over to other
// upstreams (MaxTries in total).
func (s *Server) dial(ctx context.Context, target string) (net.Conn, error) {
	var tried []string
	var lastErr error
	for i := 0; i < s.opts.MaxTries; i++ {
		var p proxy.Proxy
		cur, have := s.Current()
		switch {
		case s.opts.Rotate == RotatePerConn || !have || (i > 0 && contains(tried, cur.Key())):
			np, err := s.opts.Pick(ctx, tried)
			if err != nil {
				if lastErr == nil {
					lastErr = err
				}
				return nil, fmt.Errorf("no upstream: %w", lastErr)
			}
			p = np
			if s.opts.Rotate != RotatePerConn {
				reason := "start"
				if have {
					reason = "failover"
				}
				s.setCurrent(p, reason)
			}
		default:
			p = cur
		}
		tried = append(tried, p.Key())
		start := time.Now()
		conn, err := p.Dial(ctx, target, s.opts.DialTimeout)
		ms := max(1, int(time.Since(start).Milliseconds()))
		if err == nil {
			s.emit(Event{Kind: EventDialOK, Proxy: p, Target: target, MS: ms})
			return conn, nil
		}
		lastErr = err
		s.fails.Add(1)
		s.emit(Event{Kind: EventDialFail, Proxy: p, Target: target, MS: ms, Err: err})
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("%d upstream(s) failed for %s: %w", len(tried), target, lastErr)
}

// ---- protocol dispatch -----------------------------------------------------------

func (s *Server) handle(ctx context.Context, c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second)) // handshake budget
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	switch first[0] {
	case 5:
		s.socks5(ctx, c, br)
	case 4:
		s.socks4(ctx, c, br)
	default:
		s.http(ctx, c, br)
	}
}

// pipe copies both ways until one side closes, counting bytes.
func (s *Server) pipe(client net.Conn, br *bufio.Reader, up net.Conn) {
	_ = client.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(up, br) // br holds bytes already read from client
		s.up.Add(n)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(client, up)
		s.down.Add(n)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// ---- SOCKS5 ------------------------------------------------------------------------

func (s *Server) socks5(ctx context.Context, c net.Conn, br *bufio.Reader) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	want := byte(0x00)
	if s.opts.Auth != "" {
		want = 0x02
	}
	if !containsByte(methods, want) {
		c.Write([]byte{5, 0xff})
		return
	}
	c.Write([]byte{5, want})
	if want == 0x02 {
		// RFC 1929: VER ULEN UNAME PLEN PASSWD
		v := make([]byte, 2)
		if _, err := io.ReadFull(br, v); err != nil {
			return
		}
		user := make([]byte, v[1])
		if _, err := io.ReadFull(br, user); err != nil {
			return
		}
		pl, err := br.ReadByte()
		if err != nil {
			return
		}
		pass := make([]byte, pl)
		if _, err := io.ReadFull(br, pass); err != nil {
			return
		}
		if string(user)+":"+string(pass) != s.opts.Auth {
			c.Write([]byte{1, 1})
			return
		}
		c.Write([]byte{1, 0})
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return
	}
	reply := func(code byte) { c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}) }
	if req[1] != 1 { // CONNECT only
		reply(7)
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3:
		l, err := br.ReadByte()
		if err != nil {
			return
		}
		b := make([]byte, l)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = string(b)
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		reply(8)
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))
	up, err := s.dial(ctx, target)
	if err != nil {
		reply(1)
		return
	}
	defer up.Close()
	reply(0)
	s.pipe(c, br, up)
}

// ---- SOCKS4 / 4a -------------------------------------------------------------------------

func (s *Server) socks4(ctx context.Context, c net.Conn, br *bufio.Reader) {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	reply := func(code byte) { c.Write([]byte{0, code, 0, 0, 0, 0, 0, 0}) }
	if _, err := br.ReadString(0); err != nil { // USERID
		return
	}
	if s.opts.Auth != "" || hdr[1] != 1 {
		reply(0x5b)
		return
	}
	port := binary.BigEndian.Uint16(hdr[2:4])
	ip := net.IP(hdr[4:8])
	host := ip.String()
	if ip[0] == 0 && ip[1] == 0 && ip[2] == 0 && ip[3] != 0 { // 4a: hostname follows
		name, err := br.ReadString(0)
		if err != nil {
			return
		}
		host = strings.TrimSuffix(name, "\x00")
	}
	up, err := s.dial(ctx, net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		reply(0x5b)
		return
	}
	defer up.Close()
	reply(0x5a)
	s.pipe(c, br, up)
}

// ---- HTTP ---------------------------------------------------------------------------------

func (s *Server) http(ctx context.Context, c net.Conn, br *bufio.Reader) {
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if strings.EqualFold(hostOnly(req.Host), ControlHost) || (req.URL != nil && strings.EqualFold(req.URL.Hostname(), ControlHost)) {
		s.control(c, req)
		return
	}
	if s.opts.Auth != "" && !basicOK(req.Header.Get("Proxy-Authorization"), s.opts.Auth) {
		fmt.Fprint(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"silkrouter\"\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if req.Method == http.MethodConnect {
		target := req.Host
		if !strings.Contains(target, ":") {
			target += ":443"
		}
		up, err := s.dial(ctx, target)
		if err != nil {
			writeHTTPError(c, http.StatusBadGateway, err)
			return
		}
		defer up.Close()
		fmt.Fprint(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		s.pipe(c, br, up)
		return
	}
	// Plain request in absolute form: one request per connection.
	if req.URL == nil || req.URL.Host == "" {
		writeHTTPError(c, http.StatusBadRequest, errors.New("not a proxy request (absolute URL expected)"))
		return
	}
	target := req.URL.Host
	if req.URL.Port() == "" {
		target = net.JoinHostPort(req.URL.Hostname(), "80")
	}
	up, err := s.dial(ctx, target)
	if err != nil {
		writeHTTPError(c, http.StatusBadGateway, err)
		return
	}
	defer up.Close()
	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", "Keep-Alive"} {
		req.Header.Del(h)
	}
	req.Header.Set("Connection", "close")
	req.Close = true
	_ = c.SetDeadline(time.Time{})
	if err := req.Write(up); err != nil {
		writeHTTPError(c, http.StatusBadGateway, err)
		return
	}
	n, _ := io.Copy(c, up)
	s.down.Add(n)
}

func (s *Server) control(c net.Conn, req *http.Request) {
	if !isLoopback(c.RemoteAddr()) {
		writeHTTPError(c, http.StatusForbidden, errors.New("control API is loopback-only"))
		return
	}
	var body any
	status := http.StatusOK
	switch req.URL.Path {
	case "/status", "/":
		body = s.Status()
	case "/rotate":
		p, err := s.Rotate(context.Background(), "manual")
		if err != nil {
			status, body = http.StatusBadGateway, map[string]string{"error": err.Error()}
		} else {
			body = map[string]string{"upstream": p.Key()}
		}
	case "/stop":
		body = map[string]string{"stopping": "true"}
		defer s.Stop()
	default:
		status, body = http.StatusNotFound, map[string]string{"error": "unknown endpoint (status, rotate, stop)"}
	}
	data, _ := json.Marshal(body)
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(data), data)
}

func writeHTTPError(c net.Conn, code int, err error) {
	msg := "silkrouter relay: " + err.Error() + "\n"
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(msg), msg)
}

func basicOK(header, want string) bool {
	v, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	return err == nil && string(dec) == want
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

func isLoopback(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	return ok && ta.IP.IsLoopback()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsByte(list []byte, b byte) bool {
	for _, v := range list {
		if v == b {
			return true
		}
	}
	return false
}

// ---- client side of the control API ----------------------------------------------------

// Control sends a control request (status, rotate, stop) to the relay at
// addr and decodes the JSON answer into out (may be nil).
func Control(ctx context.Context, addr, endpoint string, out any) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	method := http.MethodPost
	if endpoint == "status" {
		method = http.MethodGet
	}
	fmt.Fprintf(c, "%s http://%s/%s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n",
		method, ControlHost, endpoint, ControlHost)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
