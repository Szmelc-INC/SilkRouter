// Package probe defines the different ways SilkRouter can validate and time a
// proxy ("cache" it). The original tool only knew Cloudflare's generate_204;
// here every method is a named Probe in a registry, so adding a new way to
// test a proxy is a one-line append.
package probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
)

// maxBody caps how much of a probe response we read (probes are tiny).
const maxBody = 64 * 1024

// ipv4Re extracts the first IPv4 address from a probe body (exit-IP probes).
var ipv4Re = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)

// Probe describes one proxy-validation method: which URL to hit and how to
// decide the response proves the proxy works.
type Probe struct {
	Name string
	Desc string
	URL  string
	// validate inspects the response; it returns the exit IP if the probe
	// echoes one, and whether the response counts as a success.
	validate func(status int, body []byte) (exitIP string, ok bool)
}

// Result is the outcome of measuring a proxy with a probe.
type Result struct {
	OK        bool
	LatencyMS int    // average round-trip over successful tries
	ExitIP    string // populated by exit-IP probes
	Err       error  // reason for failure when !OK
}

// registry holds every built-in probe keyed by name.
var registry = map[string]Probe{}

func register(p Probe) { registry[p.Name] = p }

// DefaultName is the probe used when the user does not pick one.
const DefaultName = "cloudflare"

func init() {
	// 204-style connectivity checks: fastest, no body to parse.
	register(Probe{
		Name: "cloudflare", Desc: "Cloudflare generate_204 (fast, global)",
		URL: "http://cp.cloudflare.com/generate_204", validate: expectStatus(204),
	})
	register(Probe{
		Name: "google", Desc: "Google generate_204",
		URL: "http://www.google.com/generate_204", validate: expectStatus(204),
	})
	register(Probe{
		Name: "gstatic", Desc: "Android connectivity check (gstatic 204)",
		URL: "http://connectivitycheck.gstatic.com/generate_204", validate: expectStatus(204),
	})
	register(Probe{
		Name: "msft", Desc: "Microsoft NCSI connectivity check",
		URL:      "http://www.msftconnecttest.com/connecttest.txt",
		validate: expectBody("Microsoft Connect Test"),
	})
	register(Probe{
		Name: "apple", Desc: "Apple captive portal check",
		URL:      "http://captive.apple.com/hotspot-detect.html",
		validate: expectBody("Success"),
	})
	register(Probe{
		Name: "example", Desc: "example.com (IANA reserved, very stable)",
		URL: "http://example.com/", validate: expectBody("Example Domain"),
	})
	// Exit-IP probes: also reveal the address the target sees, useful for -v
	// and anonymity checks.
	register(Probe{
		Name: "ipify", Desc: "api.ipify.org (also reports exit IP)",
		URL: "http://api.ipify.org/", validate: expectIP,
	})
	register(Probe{
		Name: "aws", Desc: "checkip.amazonaws.com (also reports exit IP)",
		URL: "http://checkip.amazonaws.com/", validate: expectIP,
	})
	register(Probe{
		Name: "icanhazip", Desc: "icanhazip.com (also reports exit IP)",
		URL: "http://icanhazip.com/", validate: expectIP,
	})
}

// Get returns the named probe, or an error listing valid names.
func Get(name string) (Probe, error) {
	if p, ok := registry[strings.ToLower(name)]; ok {
		return p, nil
	}
	return Probe{}, fmt.Errorf("unknown probe %q (see: %s)", name, strings.Join(Names(), ", "))
}

// Custom builds an ad-hoc probe from a user-supplied URL (accepts any 2xx).
func Custom(url string) Probe {
	return Probe{
		Name: "custom", Desc: "user-supplied URL", URL: url,
		validate: func(status int, body []byte) (string, bool) {
			return string(ipv4Re.Find(body)), status >= 200 && status < 300
		},
	}
}

// Names returns all built-in probe names, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// All returns every built-in probe, sorted by name.
func All() []Probe {
	out := make([]Probe, 0, len(registry))
	for _, n := range Names() {
		out = append(out, registry[n])
	}
	return out
}

// Measure runs the probe against p up to `tries` times and averages the
// latency of the successful attempts. A single success is enough to call the
// proxy alive; the error from the last failure is kept for diagnostics.
func (pr Probe) Measure(ctx context.Context, p proxy.Proxy, tries int, timeout time.Duration) Result {
	if tries < 1 {
		tries = 1
	}
	client := p.Client(timeout)
	defer client.CloseIdleConnections()

	var totalMS, ok int
	var exitIP string
	var lastErr error

	for i := 0; i < tries; i++ {
		start := time.Now()
		ip, err := pr.once(ctx, client)
		if err != nil {
			lastErr = err
			continue
		}
		totalMS += int(time.Since(start).Milliseconds())
		ok++
		if ip != "" {
			exitIP = ip
		}
	}

	if ok == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("probe failed")
		}
		return Result{OK: false, Err: lastErr}
	}
	return Result{OK: true, LatencyMS: totalMS / ok, ExitIP: exitIP}
}

// once performs a single probe request and validates the response.
func (pr Probe) once(ctx context.Context, client *http.Client) (exitIP string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pr.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "SilkRouter/2.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	ip, valid := pr.validate(resp.StatusCode, body)
	if !valid {
		return "", fmt.Errorf("unexpected response (status %d)", resp.StatusCode)
	}
	return ip, nil
}

// ---- validators -----------------------------------------------------------

func expectStatus(want int) func(int, []byte) (string, bool) {
	return func(status int, _ []byte) (string, bool) { return "", status == want }
}

func expectBody(substr string) func(int, []byte) (string, bool) {
	return func(status int, body []byte) (string, bool) {
		return "", status >= 200 && status < 400 && strings.Contains(string(body), substr)
	}
}

func expectIP(status int, body []byte) (string, bool) {
	if status < 200 || status >= 300 {
		return "", false
	}
	ip := ipv4Re.Find(body)
	return string(ip), ip != nil
}
