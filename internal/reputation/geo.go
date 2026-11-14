package reputation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Geo is what a geolocation provider knows about an IP.
type Geo struct {
	IP          string `json:"query"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"countryCode,omitempty"`
	Region      string `json:"regionName,omitempty"`
	City        string `json:"city,omitempty"`
	ISP         string `json:"isp,omitempty"`
	Org         string `json:"org,omitempty"`
	AS          string `json:"as,omitempty"` // "AS13335 Cloudflare, Inc."
	Mobile      bool   `json:"mobile"`
	Proxy       bool   `json:"proxy"`   // known proxy / VPN / Tor
	Hosting     bool   `json:"hosting"` // datacenter / hosting
}

// OK reports a successful lookup.
func (g *Geo) OK() bool { return g.Status == "success" }

// ASN returns the "AS123" part of AS.
func (g *Geo) ASN() string {
	if f := strings.Fields(g.AS); len(f) > 0 && strings.HasPrefix(f[0], "AS") {
		return f[0]
	}
	return ""
}

// Organization returns org, else ISP, else the AS name.
func (g *Geo) Organization() string {
	switch {
	case g.Org != "":
		return g.Org
	case g.ISP != "":
		return g.ISP
	}
	if _, name, ok := strings.Cut(g.AS, " "); ok {
		return name
	}
	return ""
}

const geoFields = "status,message,country,countryCode,regionName,city,isp,org,as,mobile,proxy,hosting,query"

// GeoClient talks to an ip-api compatible batch endpoint, pacing requests
// (the free ip-api.com tier allows 15 batch requests a minute, 100 IPs
// each). It is queried directly, never through a proxy.
type GeoClient struct {
	URL      string // default http://ip-api.com/batch
	Client   *http.Client
	Interval time.Duration // minimum pause between requests (0 = 4.1s)
	mu       sync.Mutex
	last     time.Time
}

// Lookup resolves ips (chunked by 100, paced). Failed lookups are left out
// of the map; err reports the first transport error.
func (c *GeoClient) Lookup(ctx context.Context, ips []string) (map[string]Geo, error) {
	out := make(map[string]Geo, len(ips))
	var firstErr error
	for start := 0; start < len(ips); start += 100 {
		end := min(start+100, len(ips))
		res, err := c.batch(ctx, ips[start:end])
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				break
			}
			continue
		}
		for _, g := range res {
			if g.OK() {
				out[g.IP] = g
			}
		}
	}
	return out, firstErr
}

func (c *GeoClient) wait(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	iv := c.Interval
	if iv <= 0 {
		iv = 4100 * time.Millisecond
	}
	if d := time.Until(c.last.Add(iv)); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	c.last = time.Now()
	return nil
}

func (c *GeoClient) batch(ctx context.Context, ips []string) ([]Geo, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	url := c.URL
	if url == "" {
		url = "http://ip-api.com/batch"
	}
	if !strings.Contains(url, "fields=") {
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		url += sep + "fields=" + geoFields
	}
	body, _ := json.Marshal(ips)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geo: %w", err)
	}
	defer resp.Body.Close()
	// Back off when the provider says the window is exhausted.
	if rl := resp.Header.Get("X-Rl"); rl == "0" {
		if ttl, err := strconv.Atoi(resp.Header.Get("X-Ttl")); err == nil && ttl > 0 {
			c.mu.Lock()
			c.last = time.Now().Add(time.Duration(ttl) * time.Second)
			c.mu.Unlock()
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("geo: rate limited (HTTP 429)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("geo: HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("geo: %w", err)
	}
	var out []Geo
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("geo: bad response: %w", err)
	}
	return out, nil
}
