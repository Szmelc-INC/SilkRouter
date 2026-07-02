package scan

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
)

const pxTargetURL = "https://whatismyipaddress.com/proxy-check"

// Proxy-detection verdicts.
const (
	PXDetected = "DETECTED" // flagged as a proxy/anonymizer
	PXClean    = "CLEAN"    // no proxy signals
	PXBlocked  = "BLOCKED"  // target served a Cloudflare challenge
	PXFail     = "FAIL"     // proxy unreachable / timed out
	PXUnknown  = "UNKNOWN"  // reachable but unparseable
)

var (
	rePXBlocked = regexp.MustCompile(`(?i)just a moment|attention required|cf-browser-verification`)
	rePXDetect  = regexp.MustCompile(`(?i)Proxy server detected`)
	rePXSeenIP  = regexp.MustCompile(`<td>IP:&nbsp;&nbsp;</td><td>([^<]+)`)
)

// PXResult is the outcome of one proxy-detection check.
type PXResult struct {
	Status string
	SeenIP string
	// Per-signal TRUE/FALSE fields; empty when not present in the response.
	RDNS, WIMIA, Tor, Loc, Header string
}

// fieldRe builds the per-label TRUE/FALSE extractor used by proxycheck.sh.
func fieldRe(label string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(label) +
		`&nbsp;Test:&nbsp;&nbsp;</td><td><span style="color:#[0-9A-Fa-f]{6};">(TRUE|FALSE)`)
}

var (
	reRDNS = fieldRe("rDNS")
	reWIM  = fieldRe("WIMIA")
	reTor  = fieldRe("Tor")
	reLoc  = fieldRe("Loc")
	reHdr  = fieldRe("Header")
)

// PXChecker routes requests through a proxy and inspects the detection page.
type PXChecker struct {
	Timeout time.Duration
}

// Check runs the proxy-detection test for p.
func (c PXChecker) Check(ctx context.Context, p proxy.Proxy) PXResult {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	// This checker must follow redirects, so use a client with default policy
	// (proxy.Client suppresses them for probing).
	client := &http.Client{Transport: p.Transport(timeout), Timeout: timeout}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pxTargetURL, nil)
	if err != nil {
		return PXResult{Status: PXFail}
	}
	req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("pragma", "no-cache")
	req.Header.Set("upgrade-insecure-requests", "1")
	req.Header.Set("user-agent", blUA)

	resp, err := client.Do(req)
	if err != nil {
		return PXResult{Status: PXFail}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || len(body) == 0 {
		return PXResult{Status: PXFail}
	}
	return parsePX(string(body))
}

// parsePX mirrors do_check() from proxycheck.sh.
func parsePX(html string) PXResult {
	if rePXBlocked.MatchString(html) {
		return PXResult{Status: PXBlocked}
	}
	res := PXResult{}
	if m := rePXSeenIP.FindStringSubmatch(html); m != nil {
		res.SeenIP = strings.TrimSpace(m[1])
	}
	res.RDNS = firstSub(reRDNS, html)
	res.WIMIA = firstSub(reWIM, html)
	res.Tor = firstSub(reTor, html)
	res.Loc = firstSub(reLoc, html)
	res.Header = firstSub(reHdr, html)

	switch {
	case rePXDetect.MatchString(html):
		res.Status = PXDetected
	case res.WIMIA != "":
		res.Status = PXClean
	default:
		res.Status = PXUnknown
	}
	return res
}

func firstSub(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// ParseTarget normalises a proxycheck target (ip | ip:port | proto://ip:port).
func ParseTarget(s string) (proxy.Proxy, error) {
	p, err := proxy.Parse(s)
	if err != nil {
		return proxy.Proxy{}, fmt.Errorf("invalid target format: %s", s)
	}
	return p, nil
}
