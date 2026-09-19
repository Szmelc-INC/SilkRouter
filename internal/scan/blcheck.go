// Package scan ports the blacklist (mxtoolbox) and proxy-detection
// (whatismyipaddress) checkers from the original bash scripts to pure Go.
package scan

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	blBase = "https://mxtoolbox.com"
	blUA   = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
)

var (
	reIPv4      = regexp.MustCompile(`(?:[0-9]{1,3}\.){3}[0-9]{1,3}`)
	reBLToken   = regexp.MustCompile(`TempAuthKey"\s*:\s*"([0-9a-fA-F-]{36})`)
	reBLTotal   = regexp.MustCompile(`against <strong>([0-9]+)`)
	reBLListed  = regexp.MustCompile(`Listed <strong>([0-9]+)`)
	reBLRow     = regexp.MustCompile(`<tr>.*?</tr>`)
	reBLName    = regexp.MustCompile(`bld_name\\">([^<]+)`)
	reWhitespal = regexp.MustCompile(`\s+`)
)

// BLResult is the outcome of one blacklist lookup.
type BLResult struct {
	IP     string
	Total  int
	Listed int
	Names  []string
	Parsed bool // false when the API response could not be parsed
}

// BLChecker performs mxtoolbox blacklist lookups. It is safe to reuse across
// many IPs; a temp token is fetched lazily on first use.
type BLChecker struct {
	Token   string
	Timeout time.Duration
	client  *http.Client
}

// NewBLChecker builds a checker with an optional pre-seeded token.
func NewBLChecker(token string, timeout time.Duration) *BLChecker {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &BLChecker{
		Token:   token,
		Timeout: timeout,
		client:  &http.Client{Timeout: timeout},
	}
}

// ExtractIP pulls the first IPv4 out of any supported line format.
func ExtractIP(line string) string {
	if i := strings.Index(line, "://"); i >= 0 {
		line = line[i+3:]
	}
	return reIPv4.FindString(line)
}

// EnsureToken fetches a temp authorization token if one is not already set.
func (c *BLChecker) EnsureToken(ctx context.Context) error {
	if c.Token != "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blBase+"/api/v1/user", nil)
	if err != nil {
		return err
	}
	req.Header.Set("user-agent", blUA)
	req.Header.Set("x-requested-with", "XMLHttpRequest")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read token response: %w", err)
	}
	m := reBLToken.FindSubmatch(body)
	if m == nil {
		return fmt.Errorf("could not obtain a temp token (pass -token or set MXTB_TOKEN)")
	}
	c.Token = string(m[1])
	return nil
}

// Lookup checks a single IP against the blacklists.
func (c *BLChecker) Lookup(ctx context.Context, ip string) (BLResult, error) {
	res := BLResult{IP: ip}
	q := url.Values{}
	q.Set("command", "blacklist")
	q.Set("argument", ip)
	q.Set("resultIndex", "6")
	q.Set("disableRhsbl", "true")
	q.Set("format", "2")
	endpoint := blBase + "/api/v1/Lookup?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return res, err
	}
	req.Header.Set("accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("content-type", "application/json; charset=utf-8")
	req.Header.Set("referer", blBase+"/SuperTool.aspx")
	req.Header.Set("tempauthorization", c.Token)
	req.Header.Set("user-agent", blUA)
	req.Header.Set("x-requested-with", "XMLHttpRequest")

	resp, err := c.client.Do(req)
	if err != nil {
		return res, fmt.Errorf("lookup %s: %w", ip, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return res, fmt.Errorf("read lookup response: %w", err)
	}
	return parseBL(ip, string(body)), nil
}

// parseBL mirrors the awk/grep logic from blcheck.sh.
func parseBL(ip, resp string) BLResult {
	res := BLResult{IP: ip}
	flat := reWhitespal.ReplaceAllString(strings.ReplaceAll(resp, "\n", " "), " ")

	m := reBLTotal.FindStringSubmatch(flat)
	if m == nil {
		return res // Parsed stays false => API error / token expired
	}
	res.Total, _ = strconv.Atoi(m[1])
	if lm := reBLListed.FindStringSubmatch(flat); lm != nil {
		res.Listed, _ = strconv.Atoi(lm[1])
	}
	for _, row := range reBLRow.FindAllString(flat, -1) {
		if !strings.Contains(row, "&nbsp;LISTED") {
			continue
		}
		if nm := reBLName.FindStringSubmatch(row); nm != nil {
			res.Names = append(res.Names, strings.TrimSpace(nm[1]))
		}
	}
	res.Parsed = true
	return res
}
