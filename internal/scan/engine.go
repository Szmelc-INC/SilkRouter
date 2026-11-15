package scan

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/reputation"
)

// Scan modes.
const (
	ModeQuick      = "quick"      // one probe per proxy: alive or dead, latency
	ModeFull       = "full"       // every check on every proxy, gently
	ModeReputation = "reputation" // IP lists + DNS blacklists only (no proxy traffic)
	ModeGeo        = "geo"        // geolocation / ASN only
	ModeProtocols  = "protocols"  // which protocols each address speaks
)

// Modes lists every scan mode.
var Modes = []string{ModeQuick, ModeFull, ModeReputation, ModeGeo, ModeProtocols}

// Probe names recorded for the non-registry checks.
const (
	ProbeTCP   = "tcp"   // raw TCP connect to the proxy
	ProbeHTTPS = "https" // TLS request through the proxy
	ProbeJudge = "judge" // anonymity judge
	ProbeProto = "proto" // protocol detection handshake
	ProbeExit  = "exit-ip"
	// ProbeHTTPS, ProbeJudge and ProbeExit are auxiliary (db.AuxProbes):
	// stored as facts, but they do not change a proxy's status or latency.
)

// Target is one proxy to scan.
type Target struct {
	ID    int64
	Proxy proxy.Proxy
	// ExitIP is the last known exit IP (reputation / geo modes use it).
	ExitIP string
}

// Sink receives the facts a scan produces; *db.Writer implements it.
type Sink interface {
	Measure(db.Measurement) error
	Reputation(db.Reputation) error
	Info(db.ProxyInfo) error
}

// Options configure an Engine.
type Options struct {
	Mode     string
	Workers  int
	Tries    int
	Timeout  time.Duration
	Delay    time.Duration // pause between two checks of one proxy
	Probes   []probe.Probe // full: all; quick: the first
	FailFast int           // full: stop after this many probes failed completely (0 = never)

	ExitProbe *probe.Probe   // nil = skip
	HTTPSURLs []string       // https check: primary URL, then fallbacks (empty = skip)
	TLSRoots  *x509.CertPool // certificate roots for the https check (nil = system)
	JudgeURL  string         // "" = skip
	RealIP    string         // your real IP, for transparent-proxy detection
	Protocols bool

	DNSBL     *reputation.DNSBL // nil = skip
	Lists     []*reputation.Set // nil = skip
	Geo       *reputation.GeoClient
	MXTB      *BLChecker // nil = skip
	MXTBDelay time.Duration
	WIMIA     *PXChecker // nil = skip

	// ProtoTarget is the host:port protocol detection tunnels to
	// (default cp.cloudflare.com:80, which answers /generate_204).
	ProtoTarget string

	// OnDone is called once per target, from a single goroutine, in
	// completion order.
	OnDone func(Report)
}

// Report summarises one scanned target.
type Report struct {
	Target    Target        `json:"-"`
	ID        int64         `json:"id"`
	URL       string        `json:"url"`
	Alive     bool          `json:"alive"`
	MS        int           `json:"ms,omitempty"`     // median latency of successful samples
	Jitter    float64       `json:"jitter,omitempty"` // stddev of the samples
	ProbesOK  int           `json:"probes_ok"`
	ProbesRun int           `json:"probes_run"`
	ExitIP    string        `json:"exit_ip,omitempty"`
	Country   string        `json:"country,omitempty"`
	HTTPS     *bool         `json:"https,omitempty"`
	MITM      *bool         `json:"tls_mitm,omitempty"` // TLS interception detected
	TLSIssuer string        `json:"tls_issuer,omitempty"`
	Anonymity string        `json:"anonymity,omitempty"`
	Protocols []string      `json:"protocols,omitempty"`
	Listed    int           `json:"listed"`  // lists / zones that list host or exit IP
	Checked   int           `json:"checked"` // lists / zones answered
	ListedOn  []string      `json:"listed_on,omitempty"`
	ErrClass  string        `json:"err_class,omitempty"`
	Elapsed   time.Duration `json:"elapsed_ns"`

	interrupted bool
}

// Stats are live counters of a running scan.
type Stats struct {
	Total, Done, Alive, Dead, Listed int64
	Started                          time.Time
}

// Engine runs a scan.
type Engine struct {
	opts Options
	sink Sink

	repCache reputation.Cache[[]db.Reputation]
	geoCache sync.Map // ip -> reputation.Geo
	mxtbMu   sync.Mutex
	geoQueue chan geoReq
	geoDone  chan struct{}
	stats    struct{ total, done, alive, dead, listed atomic.Int64 }
	started  time.Time
}

type geoReq struct {
	proxyID int64
	ip      string
}

// New returns an engine writing into sink.
func New(opts Options, sink Sink) *Engine {
	if opts.Workers <= 0 {
		opts.Workers = 16
	}
	if opts.Tries <= 0 {
		opts.Tries = 1
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.ProtoTarget == "" {
		opts.ProtoTarget = "cp.cloudflare.com:80"
	}
	if opts.Mode == "" {
		opts.Mode = ModeFull
	}
	return &Engine{opts: opts, sink: sink}
}

// Stats returns the live counters.
func (e *Engine) Stats() Stats {
	return Stats{
		Total: e.stats.total.Load(), Done: e.stats.done.Load(), Alive: e.stats.alive.Load(),
		Dead: e.stats.dead.Load(), Listed: e.stats.listed.Load(), Started: e.started,
	}
}

// Run scans targets and returns when every target is done or ctx is
// cancelled (in-flight checks stop at their next step; their partial facts
// are kept). Geolocation lookups still pending are flushed before Run
// returns unless ctx is cancelled.
func (e *Engine) Run(ctx context.Context, targets []Target) error {
	e.started = time.Now()
	e.stats.total.Store(int64(len(targets)))
	if e.opts.Geo != nil && (e.opts.Mode == ModeFull || e.opts.Mode == ModeGeo) {
		e.geoQueue = make(chan geoReq, 256)
		e.geoDone = make(chan struct{})
		go e.geoLoop(ctx)
	}
	jobs := make(chan Target)
	reports := make(chan Report, e.opts.Workers)
	var wg sync.WaitGroup
	for i := 0; i < e.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				r := e.scanOne(ctx, t)
				if ctx.Err() != nil && (r.interrupted || (r.ProbesRun == 0 && r.Checked == 0)) {
					continue // interrupted: not done, a resumed scan redoes it
				}
				reports <- r
			}
		}()
	}
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for r := range reports {
			e.stats.done.Add(1)
			if r.Alive {
				e.stats.alive.Add(1)
			} else if r.ProbesRun > 0 {
				e.stats.dead.Add(1)
			}
			if r.Listed > 0 {
				e.stats.listed.Add(1)
			}
			if e.opts.OnDone != nil {
				e.opts.OnDone(r)
			}
		}
	}()
feed:
	for _, t := range targets {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- t:
		}
	}
	close(jobs)
	wg.Wait()
	close(reports)
	<-collected
	if e.geoQueue != nil {
		close(e.geoQueue)
		<-e.geoDone
	}
	return ctx.Err()
}

func (e *Engine) pause(ctx context.Context) bool {
	if e.opts.Delay <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(e.opts.Delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (e *Engine) measure(m db.Measurement) {
	if m.Kind == "" {
		m.Kind = db.KindScan
	}
	_ = e.sink.Measure(m)
}

// scanOne runs the checks of the engine's mode on one target.
func (e *Engine) scanOne(ctx context.Context, t Target) Report {
	start := time.Now()
	r := Report{Target: t, ID: t.ID, URL: t.Proxy.Key(), ExitIP: t.ExitIP}
	switch e.opts.Mode {
	case ModeQuick:
		e.probes(ctx, t, &r, e.opts.Probes[:min(1, len(e.opts.Probes))], false)
	case ModeReputation:
		e.reputation(ctx, t, &r, true)
	case ModeGeo:
		e.enqueueGeo(t, r.ExitIP)
	case ModeProtocols:
		e.protocols(ctx, t, &r)
		r.Alive = len(r.Protocols) > 0
	default:
		e.full(ctx, t, &r)
	}
	r.interrupted = ctx.Err() != nil
	r.Elapsed = time.Since(start)
	return r
}

func (e *Engine) full(ctx context.Context, t Target, r *Report) {
	p := t.Proxy
	tcpMS, err := tcpConnect(ctx, p, e.opts.Timeout)
	if err != nil {
		r.ProbesRun++
		r.ErrClass = probe.ClassifyErr(err)
		e.measure(db.Measurement{ProxyID: t.ID, Probe: ProbeTCP, OK: false, ErrClass: r.ErrClass,
			Detail: jsonDetail(map[string]any{"error": err.Error()})})
		// The local lists need no traffic at all: still learn about the
		// host (DNS blacklists are kept for proxies that answer).
		e.reputation(ctx, t, r, false)
		e.info(t, r, nil)
		return
	}
	if !e.pause(ctx) {
		return
	}
	samples := e.probes(ctx, t, r, e.opts.Probes, true, tcpMS)
	if !r.Alive || ctx.Err() != nil {
		if e.opts.Protocols && ctx.Err() == nil {
			e.protocols(ctx, t, r) // maybe it speaks another protocol
		}
		e.reputation(ctx, t, r, false)
		e.info(t, r, samples)
		return
	}
	if e.opts.ExitProbe != nil && e.pause(ctx) {
		res := e.opts.ExitProbe.Measure(ctx, p, 1, e.opts.Timeout)
		e.measure(db.Measurement{ProxyID: t.ID, Probe: ProbeExit, OK: res.OK, MS: res.LatencyMS,
			ErrClass: probe.ClassifyErr(res.Err), ExitIP: res.ExitIP,
			Detail: jsonDetail(map[string]any{"probe": e.opts.ExitProbe.Name})})
		if res.ExitIP != "" {
			r.ExitIP = res.ExitIP
		}
	}
	if len(e.opts.HTTPSURLs) > 0 && e.pause(ctx) {
		e.https(ctx, t, r)
	}
	if e.opts.JudgeURL != "" && e.pause(ctx) {
		e.judge(ctx, t, r)
	}
	if e.opts.Protocols && e.pause(ctx) {
		e.protocols(ctx, t, r)
	}
	if e.opts.WIMIA != nil && e.pause(ctx) {
		e.wimia(ctx, t, r)
	}
	e.reputation(ctx, t, r, true)
	e.enqueueGeo(t, r.ExitIP)
	e.info(t, r, samples)
}

// info writes what the scan learned beyond measurements.
func (e *Engine) info(t Target, r *Report, samples []int) {
	in := db.ProxyInfo{ProxyID: t.ID, Anonymity: r.Anonymity, HTTPSOK: r.HTTPS, TLSMITM: r.MITM}
	if r.MITM != nil && *r.MITM {
		in.TLSIssuer = r.TLSIssuer
	}
	if e.opts.Mode == ModeFull {
		in.FullScanAt = time.Now().UTC()
	}
	if r.Protocols != nil {
		in.Protocols = r.Protocols
	}
	if len(samples) >= 2 {
		j := stddev(samples)
		r.Jitter = j
		in.JitterMS = &j
	}
	if r.Country != "" && e.opts.Geo == nil {
		in.Country = r.Country
	}
	_ = e.sink.Info(in)
}

// probes runs each probe Tries times. A probe counts as ok when one sample
// succeeded; its measurement carries the median of the successful samples.
// It returns every successful sample (for jitter).
func (e *Engine) probes(ctx context.Context, t Target, r *Report, probes []probe.Probe, gentle bool, tcpMS ...int) []int {
	var all []int
	failedProbes := 0
	for i, pr := range probes {
		if ctx.Err() != nil {
			break
		}
		if e.opts.FailFast > 0 && failedProbes >= e.opts.FailFast && r.ProbesOK == 0 {
			break
		}
		var ok []int
		var lastErr error
		var exitIP string
		for try := 0; try < e.opts.Tries; try++ {
			if try > 0 && gentle && !e.pause(ctx) {
				break
			}
			res := pr.Measure(ctx, t.Proxy, 1, e.opts.Timeout)
			if res.OK {
				ok = append(ok, res.LatencyMS)
				if res.ExitIP != "" {
					exitIP = res.ExitIP
				}
			} else {
				lastErr = res.Err
				if len(ok) == 0 && try == 0 && failedProbes > 0 {
					break // a second probe failing on its first try: don't hammer
				}
			}
		}
		r.ProbesRun++
		detail := map[string]any{"tries": e.opts.Tries, "ok": len(ok), "samples": ok}
		if i == 0 && len(tcpMS) > 0 {
			detail["tcp_ms"] = tcpMS[0]
		}
		m := db.Measurement{ProxyID: t.ID, Probe: pr.Name, OK: len(ok) > 0, ExitIP: exitIP, Detail: jsonDetail(detail)}
		if len(ok) > 0 {
			m.MS = median(ok)
			r.ProbesOK++
			all = append(all, ok...)
			if exitIP != "" {
				r.ExitIP = exitIP
			}
		} else {
			m.ErrClass = probe.ClassifyErr(lastErr)
			if r.ErrClass == "" {
				r.ErrClass = m.ErrClass
			}
			failedProbes++
		}
		e.measure(m)
		if gentle && i < len(probes)-1 && !e.pause(ctx) {
			break
		}
	}
	r.Alive = r.ProbesOK > 0
	if r.Alive {
		r.MS = median(all)
		r.ErrClass = ""
	}
	return all
}

// https checks TLS through the proxy. For each URL (https_url, then the
// fallbacks) it tunnels to host:443, runs the TLS handshake itself and
// verifies the certificate chain for that host. A certificate that does not
// verify means the proxy intercepts TLS (it terminates the connection and
// re-signs it): that is recorded (mitm, issuer) and no fallback is tried,
// since it is a property of the proxy. Otherwise a GET is sent over the
// verified connection; the first URL answering 2xx / 3xx passes. A
// Cloudflare trace body ("ip=...\nloc=PL\n...") also yields exit IP and
// country. Every attempt is an auxiliary fact (probe "https").
func (e *Engine) https(ctx context.Context, t Target, r *Report) {
	ok := false
	for i, raw := range e.opts.HTTPSURLs {
		if i > 0 && !e.pause(ctx) {
			break
		}
		res := TLSFetch(ctx, t.Proxy, raw, e.opts.Timeout, e.opts.TLSRoots)
		m := db.Measurement{ProxyID: t.ID, Probe: ProbeHTTPS, OK: res.OK}
		detail := map[string]any{"url": raw, "status": res.Status}
		if res.Verified != nil {
			detail["verified"] = *res.Verified
		}
		if res.OK {
			m.MS = res.MS
			kv := parseTrace(res.Body)
			if ip := kv["ip"]; ip != "" {
				m.ExitIP = ip
				if r.ExitIP == "" {
					r.ExitIP = ip
				}
			}
			if loc := kv["loc"]; ValidCountry(loc) {
				r.Country = loc
			}
			for _, k := range []string{"loc", "colo", "tls", "http", "warp"} {
				if kv[k] != "" {
					detail[k] = kv[k]
				}
			}
		} else {
			m.ErrClass = res.ErrClass
			if res.Err != nil {
				detail["error"] = res.Err.Error()
			}
		}
		if res.MITM {
			detail["mitm"] = true
			detail["issuer"] = res.Issuer
			detail["subject"] = res.Subject
		}
		m.Detail = jsonDetail(detail)
		e.measure(m)
		if res.Verified != nil {
			mitm := res.MITM
			r.MITM = &mitm
			r.TLSIssuer = res.Issuer
		}
		if res.OK || res.MITM || ctx.Err() != nil {
			ok = res.OK
			break
		}
	}
	r.HTTPS = &ok
}

// TLSResult is the outcome of TLSFetch.
type TLSResult struct {
	OK       bool
	MS       int
	Status   int
	Body     string
	Verified *bool // nil when the handshake did not get that far
	MITM     bool  // the certificate does not verify for the host
	Issuer   string
	Subject  string
	Err      error
	ErrClass string
}

// TLSFetch GETs rawURL through p with its own TLS handshake: the chain is
// verified against roots (nil = the system pool) for the URL's host.
func TLSFetch(ctx context.Context, p proxy.Proxy, rawURL string, timeout time.Duration, roots *x509.CertPool) TLSResult {
	var res TLSResult
	fail := func(err error, class string) TLSResult {
		res.Err, res.ErrClass = err, class
		if class == "" {
			res.ErrClass = probe.ClassifyErr(err)
		}
		return res
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fail(fmt.Errorf("bad https URL %q", rawURL), db.ErrOther)
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	start := time.Now()
	conn, err := p.Dial(ctx, net.JoinHostPort(host, port), timeout)
	if err != nil {
		return fail(err, "")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	tc := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"}})
	if err := tc.HandshakeContext(ctx); err != nil {
		return fail(fmt.Errorf("tls handshake: %w", err), db.ErrTLS)
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return fail(errors.New("tls: no certificate presented"), db.ErrTLS)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	verified := true
	if _, err := certs[0].Verify(x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: inter}); err != nil {
		verified = false
		res.Verified, res.MITM = &verified, true
		res.Issuer, res.Subject = certName(certs[0].Issuer), certName(certs[0].Subject)
		return fail(fmt.Errorf("tls interception: certificate for %s not trusted (issuer %q): %w", host, res.Issuer, err), db.ErrTLS)
	}
	res.Verified = &verified
	res.Issuer, res.Subject = certName(certs[0].Issuer), certName(certs[0].Subject)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fail(err, db.ErrOther)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0")
	req.Header.Set("Accept", "*/*")
	req.Close = true
	if err := req.Write(tc); err != nil {
		return fail(err, "")
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		return fail(err, "")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	res.Status, res.Body = resp.StatusCode, string(data)
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fail(fmt.Errorf("unexpected status %d", resp.StatusCode), db.ErrHTTPStatus)
	}
	res.OK, res.MS = true, max(1, int(time.Since(start).Milliseconds()))
	return res
}

// certName renders a certificate name compactly: "CN (O)".
func certName(n pkix.Name) string {
	cn := n.CommonName
	org := strings.Join(n.Organization, ", ")
	switch {
	case cn != "" && org != "":
		return cn + " (" + org + ")"
	case cn != "":
		return cn
	case org != "":
		return org
	}
	return n.String()
}

// proxyHeaders reveal that a request went through a proxy.
var proxyHeaders = []string{"via", "x-forwarded-for", "forwarded", "x-real-ip", "client-ip", "x-client-ip",
	"x-proxy-id", "proxy-connection", "x-forwarded-host", "forwarded-for", "x-cluster-client-ip",
	"x-originating-ip", "x-bluecoat-via", "x-proxy-connection", "http_via", "http_x_forwarded_for",
	"http_forwarded", "http_client_ip"}

// Grade classifies a judge response: transparent when it shows realIP,
// anonymous when it shows proxy headers (or several origin IPs), elite
// otherwise. It returns the revealing header names it saw.
func Grade(body, realIP string) (string, []string) {
	lower := strings.ToLower(body)
	var seen []string
	var origin string
	var js struct {
		Headers map[string]any `json:"headers"`
		Origin  string         `json:"origin"`
	}
	if json.Unmarshal([]byte(body), &js) == nil && js.Headers != nil {
		origin = js.Origin
		for k := range js.Headers {
			for _, h := range proxyHeaders {
				if strings.EqualFold(k, h) {
					seen = append(seen, strings.ToLower(k))
				}
			}
		}
	} else {
		for _, h := range proxyHeaders {
			if strings.Contains(lower, h+":") || strings.Contains(lower, h+" =") || strings.Contains(lower, "\""+h+"\"") {
				seen = append(seen, h)
			}
		}
	}
	sort.Strings(seen)
	if realIP != "" && strings.Contains(body, realIP) {
		return "transparent", seen
	}
	if len(seen) > 0 || strings.Contains(origin, ",") {
		return "anonymous", seen
	}
	return "elite", seen
}

func (e *Engine) judge(ctx context.Context, t Target, r *Report) {
	client := t.Proxy.Client(e.opts.Timeout)
	defer client.CloseIdleConnections()
	start := time.Now()
	body, status, err := get(ctx, client, e.opts.JudgeURL)
	ms := max(1, int(time.Since(start).Milliseconds()))
	m := db.Measurement{ProxyID: t.ID, Probe: ProbeJudge}
	if err != nil || status < 200 || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
			m.ErrClass = db.ErrHTTPStatus
		} else {
			m.ErrClass = probe.ClassifyErr(err)
		}
		m.Detail = jsonDetail(map[string]any{"error": err.Error()})
		e.measure(m)
		return
	}
	grade, headers := Grade(body, e.opts.RealIP)
	r.Anonymity = grade
	m.OK, m.MS = true, ms
	m.Detail = jsonDetail(map[string]any{"anonymity": grade, "headers": headers, "real_ip_known": e.opts.RealIP != ""})
	e.measure(m)
}

// protoSchemes are the handshakes protocol detection tries.
var protoSchemes = []proxy.Scheme{proxy.HTTP, proxy.SOCKS5, proxy.SOCKS4}

// sameFamily reports whether s is the handshake family of the target's own
// scheme (https proxies speak the http CONNECT handshake, socks4a socks4).
func sameFamily(s, own proxy.Scheme) bool {
	return s == own || (s == proxy.HTTP && own == proxy.HTTPS) || (s == proxy.SOCKS4 && own == proxy.SOCKS4A)
}

// protocols tries every handshake on host:port and fetches a 204 through
// the tunnel. The target's own scheme counts as verified without another
// test when its probes already succeeded (many free proxies throttle a
// burst of connections, so re-testing it last gives false negatives). A
// scheme other than the target's that works is recorded as a measurement
// of that address (which adds it to the database).
func (e *Engine) protocols(ctx context.Context, t Target, r *Report) {
	var works []string
	tested := 0
	for _, s := range protoSchemes {
		if ctx.Err() != nil {
			return
		}
		own := sameFamily(s, t.Proxy.Scheme)
		if own && r.ProbesOK > 0 {
			works = append(works, string(s))
			continue
		}
		if tested > 0 && !e.pause(ctx) {
			return
		}
		tested++
		pp := t.Proxy
		pp.Scheme = s
		ms, err := tunnelCheck(ctx, pp, e.opts.ProtoTarget, e.opts.Timeout)
		if err != nil {
			continue
		}
		works = append(works, string(s))
		if own {
			continue
		}
		e.measure(db.Measurement{Addr: db.AddrOf(pp), Probe: ProbeProto, OK: true, MS: ms,
			Detail: jsonDetail(map[string]any{"found_via": t.Proxy.Key()})})
	}
	if works == nil {
		works = []string{}
	}
	r.Protocols = works
}

// reputation checks host and exit IP against the lists and (with remote)
// the DNS blacklists and mxtoolbox, each IP once per scan.
func (e *Engine) reputation(ctx context.Context, t Target, r *Report, remote bool) {
	var ips []string
	if a, err := netip.ParseAddr(t.Proxy.Host); err == nil {
		ips = append(ips, a.String())
	}
	if r.ExitIP != "" && (len(ips) == 0 || r.ExitIP != ips[0]) {
		ips = append(ips, r.ExitIP)
	}
	listed := map[string]bool{}
	for _, ip := range ips {
		if ctx.Err() != nil {
			return
		}
		key := ip
		if remote {
			key += "|remote"
		}
		rows, _ := e.repCache.Get(key, func() []db.Reputation { return e.checkIP(ctx, ip, remote) })
		for _, row := range rows {
			r.Checked++
			if row.Listed && !listed[row.ListName] {
				listed[row.ListName] = true
				r.ListedOn = append(r.ListedOn, row.ListName)
			}
		}
	}
	sort.Strings(r.ListedOn)
	r.Listed = len(r.ListedOn)
}

// checkIP produces (and writes) every verdict about one IP.
func (e *Engine) checkIP(ctx context.Context, ip string, remote bool) []db.Reputation {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil
	}
	var rows []db.Reputation
	now := time.Now().UTC()
	for _, m := range reputation.MatchAll(e.opts.Lists, addr) {
		rows = append(rows, db.Reputation{IP: ip, ListName: m.List, ListKind: m.Kind, Listed: m.Listed, CheckedAt: now})
	}
	if !remote {
		for _, row := range rows {
			_ = e.sink.Reputation(row)
		}
		return rows
	}
	if e.opts.DNSBL != nil {
		for _, res := range e.opts.DNSBL.Check(ctx, addr) {
			if res.Err != nil {
				continue // unknown: no verdict
			}
			rows = append(rows, db.Reputation{IP: ip, ListName: res.Zone, ListKind: reputation.KindDNSBL,
				Listed: res.Listed, Detail: strings.Join(res.Codes, ","), CheckedAt: now})
		}
	}
	if e.opts.MXTB != nil && ctx.Err() == nil {
		e.mxtbMu.Lock()
		res, err := e.opts.MXTB.Lookup(ctx, ip)
		if e.opts.MXTBDelay > 0 {
			time.Sleep(e.opts.MXTBDelay)
		}
		e.mxtbMu.Unlock()
		if err == nil && res.Parsed {
			rows = append(rows, db.Reputation{IP: ip, ListName: "mxtoolbox", ListKind: reputation.KindAPI,
				Listed: res.Listed > 0, Detail: fmt.Sprintf("%d/%d %s", res.Listed, res.Total, strings.Join(res.Names, ", ")),
				CheckedAt: now})
		}
	}
	for _, row := range rows {
		_ = e.sink.Reputation(row)
	}
	return rows
}

func (e *Engine) wimia(ctx context.Context, t Target, r *Report) {
	res := e.opts.WIMIA.Check(ctx, t.Proxy)
	ip := res.SeenIP
	if ip == "" {
		ip = r.ExitIP
	}
	if ip == "" || (res.Status != PXDetected && res.Status != PXClean) {
		return
	}
	_ = e.sink.Reputation(db.Reputation{IP: ip, ListName: "wimia", ListKind: reputation.KindAPI,
		Listed: res.Status == PXDetected, Detail: fmt.Sprintf("rdns=%s wimia=%s tor=%s loc=%s header=%s",
			res.RDNS, res.WIMIA, res.Tor, res.Loc, res.Header)})
}

// ---- geolocation (batched) ----------------------------------------------------

func (e *Engine) enqueueGeo(t Target, exitIP string) {
	if e.geoQueue == nil {
		return
	}
	ip := exitIP
	if ip == "" {
		ip = t.Proxy.Host
	}
	if _, err := netip.ParseAddr(ip); err != nil {
		return
	}
	if g, ok := e.geoCache.Load(ip); ok {
		e.applyGeo(t.ID, g.(reputation.Geo))
		return
	}
	e.geoQueue <- geoReq{proxyID: t.ID, ip: ip}
}

func (e *Engine) geoLoop(ctx context.Context) {
	defer close(e.geoDone)
	pending := map[string][]int64{}
	var order []string
	flush := func() {
		if len(order) == 0 {
			return
		}
		res, _ := e.opts.Geo.Lookup(ctx, order)
		for _, ip := range order {
			g, ok := res[ip]
			if !ok {
				continue
			}
			e.geoCache.Store(ip, g)
			for _, id := range pending[ip] {
				e.applyGeo(id, g)
			}
			now := time.Now().UTC()
			_ = e.sink.Reputation(db.Reputation{IP: ip, ListName: "ip-api:proxy", ListKind: reputation.KindAPI, Listed: g.Proxy, CheckedAt: now})
			_ = e.sink.Reputation(db.Reputation{IP: ip, ListName: "ip-api:hosting", ListKind: reputation.KindDatacenter, Listed: g.Hosting, CheckedAt: now})
		}
		pending, order = map[string][]int64{}, nil
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case req, ok := <-e.geoQueue:
			if !ok {
				if ctx.Err() == nil {
					flush()
				}
				return
			}
			if g, ok := e.geoCache.Load(req.ip); ok {
				e.applyGeo(req.proxyID, g.(reputation.Geo))
				continue
			}
			if _, ok := pending[req.ip]; !ok {
				order = append(order, req.ip)
			}
			pending[req.ip] = append(pending[req.ip], req.proxyID)
			if len(order) >= 100 {
				flush()
			}
		case <-tick.C:
			flush()
		case <-ctx.Done():
			// Drain without lookups so senders never block.
			for range e.geoQueue {
			}
			return
		}
	}
}

func (e *Engine) applyGeo(id int64, g reputation.Geo) {
	cc := g.CountryCode
	if !ValidCountry(cc) {
		cc = "" // ip-api is plain HTTP: never store what a MITM could inject
	}
	_ = e.sink.Info(db.ProxyInfo{ProxyID: id, Country: cc, Region: g.Region, City: g.City,
		ASN: g.ASN(), Org: g.Organization(), ISP: g.ISP, Hosting: db.Bool(g.Hosting), Mobile: db.Bool(g.Mobile)})
}

// ValidCountry reports whether cc is a two-letter ASCII country code. The
// trace body comes through the proxy under test and the geo lookup is plain
// HTTP, so both are untrusted; the code ends up in prompts and tables.
func ValidCountry(cc string) bool {
	return len(cc) == 2 && isAlpha(cc[0]) && isAlpha(cc[1])
}

func isAlpha(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

// ---- low-level checks -----------------------------------------------------------

func tcpConnect(ctx context.Context, p proxy.Proxy, timeout time.Duration) (int, error) {
	d := net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", p.Addr())
	if err != nil {
		return 0, err
	}
	conn.Close()
	return max(1, int(time.Since(start).Milliseconds())), nil
}

// tunnelCheck opens a tunnel through p to target and expects an HTTP reply
// to a GET /generate_204.
func tunnelCheck(ctx context.Context, p proxy.Proxy, target string, timeout time.Duration) (int, error) {
	start := time.Now()
	conn, err := p.Dial(ctx, target, timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	host, _, _ := net.SplitHostPort(target)
	if _, err := fmt.Fprintf(conn, "GET /generate_204 HTTP/1.1\r\nHost: %s\r\nUser-Agent: SilkRouter/4\r\nConnection: close\r\n\r\n", host); err != nil {
		return 0, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(line, "HTTP/1.") {
		return 0, fmt.Errorf("not an HTTP reply: %q", strings.TrimSpace(line))
	}
	return max(1, int(time.Since(start).Milliseconds())), nil
}

func get(ctx context.Context, client *http.Client, url string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0")
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return string(data), resp.StatusCode, err
}

// RealIP fetches url directly (no proxy) and returns the first IPv4/IPv6
// address in the body.
func RealIP(ctx context.Context, url string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	body, status, err := get(ctx, client, url)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("real IP: HTTP %d", status)
	}
	for _, f := range strings.FieldsFunc(body, func(r rune) bool {
		return !(r == '.' || r == ':' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'))
	}) {
		if a, err := netip.ParseAddr(f); err == nil {
			return a.String(), nil
		}
	}
	return "", fmt.Errorf("real IP: no address in the response")
}

func parseTrace(body string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			out[k] = v
		}
	}
	return out
}

func median(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2] + 1) / 2
}

func stddev(xs []int) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += float64(x)
	}
	mean := sum / float64(len(xs))
	var v float64
	for _, x := range xs {
		v += (float64(x) - mean) * (float64(x) - mean)
	}
	return math.Round(math.Sqrt(v/float64(len(xs)-1))*10) / 10
}

func jsonDetail(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
