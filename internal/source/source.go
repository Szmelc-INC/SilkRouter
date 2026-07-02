// Package source gathers raw proxy candidates from one of three origins: a
// remote URL, a local file, or the list embedded in the binary. It parses and
// de-duplicates them into proxy.Proxy values ready for testing.
package source

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
)

// DefaultURL is the upstream consolidated list shipped with the project.
const DefaultURL = "https://raw.githubusercontent.com/Szmelc-INC/SilkRouter/refs/heads/main/proxy/LIST.txt"

// Builtin holds the proxy list embedded into the binary. main wires this up
// from the //go:embed of proxy/LIST.txt so `--source builtin` works offline.
var Builtin []byte

// Origin selects where candidates come from. Exactly one of the fields drives
// the fetch; File wins over URL, and UseBuiltin wins over both.
type Origin struct {
	URL        string
	File       string
	UseBuiltin bool
}

// Describe returns a short human label for the origin (used in log lines).
func (o Origin) Describe() string {
	switch {
	case o.UseBuiltin:
		return "embedded list"
	case o.File != "":
		return "file " + o.File
	case o.URL != "":
		return "url " + o.URL
	default:
		return "url " + DefaultURL
	}
}

// Raw returns the undecoded candidate text for the origin.
func (o Origin) Raw(ctx context.Context, timeout time.Duration) ([]byte, error) {
	switch {
	case o.UseBuiltin:
		if len(Builtin) == 0 {
			return nil, fmt.Errorf("no embedded proxy list built into this binary")
		}
		return Builtin, nil
	case o.File != "":
		data, err := os.ReadFile(o.File)
		if err != nil {
			return nil, fmt.Errorf("read source file: %w", err)
		}
		return data, nil
	default:
		url := o.URL
		if url == "" {
			url = DefaultURL
		}
		return fetchURL(ctx, url, timeout)
	}
}

// Candidates fetches, parses and de-duplicates the origin into proxies.
func (o Origin) Candidates(ctx context.Context, timeout time.Duration) ([]proxy.Proxy, error) {
	raw, err := o.Raw(ctx, timeout)
	if err != nil {
		return nil, err
	}
	return ParseLines(raw), nil
}

// ParseLines parses newline-separated proxy text, skipping blanks/comments and
// unparseable junk, de-duplicating by scheme://host:port.
func ParseLines(raw []byte) []proxy.Proxy {
	seen := make(map[string]struct{})
	var out []proxy.Proxy
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimRight(sc.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := proxy.Parse(line)
		if err != nil {
			continue
		}
		if _, dup := seen[p.Key()]; dup {
			continue
		}
		seen[p.Key()] = struct{}{}
		out = append(out, p)
	}
	return out
}

// fetchURL downloads the list, following redirects, with a hard timeout.
func fetchURL(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "SilkRouter/2.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("fetch %s: empty response", url)
	}
	return data, nil
}
