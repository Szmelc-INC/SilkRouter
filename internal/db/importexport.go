package db

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/source"
)

// Source labels used by the automatic import.
const (
	SourceBuiltin = "builtin"
	SourceCache   = "cache"
)

// FirstRunOptions configure FirstRun.
type FirstRunOptions struct {
	// Builtin is the embedded proxy list (source.Builtin); empty = skip.
	Builtin []byte
	// CacheFile is the legacy text cache (<home>/cache.list); its entries
	// are imported as alive with their latencies as 'legacy' probe facts.
	// Missing file = skip.
	CacheFile string
}

// FirstRunResult reports what FirstRun imported.
type FirstRunResult struct {
	Ran     bool          `json:"ran"` // false: the database already had proxies
	Builtin ImportResult  `json:"builtin"`
	Cache   ImportResult  `json:"cache"`
	Elapsed time.Duration `json:"elapsed_ns"`
}

// FirstRun fills an empty database: the embedded list (source "builtin")
// and the legacy text cache (source "cache"). The emptiness check and the
// imports share one immediate transaction, so two processes starting at
// once cannot both import.
func (d *DB) FirstRun(ctx context.Context, opts FirstRunOptions) (FirstRunResult, error) {
	start := time.Now()
	var res FirstRunResult
	// Fast path without a write lock: only an empty database goes on.
	var n int
	if err := d.sql.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM proxies)`).Scan(&n); err != nil {
		return res, err
	}
	if n > 0 {
		return res, nil
	}
	builtin := source.ParseLines(opts.Builtin)
	cacheProxies, cacheErr := loadCacheFile(opts.CacheFile)
	if cacheErr != nil {
		return res, cacheErr
	}
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM proxies`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		res.Ran = true
		var err error
		if len(builtin) > 0 {
			if res.Builtin, err = importTx(ctx, tx, builtin, ImportOptions{Source: SourceBuiltin}); err != nil {
				return err
			}
		}
		if len(cacheProxies) > 0 {
			if res.Cache, err = importTx(ctx, tx, cacheProxies, ImportOptions{Source: SourceCache, AllAlive: true}); err != nil {
				return err
			}
		}
		now := strconv.FormatInt(nowFunc().UnixMilli(), 10)
		for k, v := range map[string]string{MetaFirstImport: now, MetaBuiltinHash: hashOf(opts.Builtin)} {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
	res.Elapsed = time.Since(start)
	return res, err
}

func hashOf(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// loadCacheFile parses a legacy text cache; a missing or empty path yields
// nothing.
func loadCacheFile(path string) ([]proxy.Proxy, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: read %s: %w", path, err)
	}
	return source.ParseLines(data), nil
}

// ---- export ------------------------------------------------------------------

// Export formats.
const (
	FormatList = "list" // legacy text cache: scheme://host:port [Nms], fastest first
	FormatURLs = "urls" // one URL per line, no latency
	FormatCSV  = "csv"
	FormatJSON = "json"
)

// ExportFormats lists the accepted Export formats.
var ExportFormats = []string{FormatList, FormatURLs, FormatCSV, FormatJSON}

// Export writes the proxies matching f to w. The list format is the legacy
// text cache (latency = last_ms, fastest first unless f.Sort says
// otherwise), so `px db export > cache.list` stays interchangeable with v3
// caches and px.sh. It returns the number of proxies written.
func (d *DB) Export(ctx context.Context, w io.Writer, format string, f Filter) (int, error) {
	if f.Sort == "" && (format == FormatList || format == FormatURLs) {
		f.Sort = "last_ms"
	}
	ps, err := d.ListProxies(ctx, f)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriter(w)
	switch format {
	case FormatList, "":
		for _, p := range ps {
			fmt.Fprintln(bw, p.ToProxy().String())
		}
	case FormatURLs:
		for _, p := range ps {
			fmt.Fprintln(bw, p.URL())
		}
	case FormatCSV:
		cw := csv.NewWriter(bw)
		cw.Write([]string{"id", "url", "scheme", "host", "port", "status", "last_ms", "best_ms", "avg_ms",
			"ok_count", "fail_count", "ok_streak", "fail_streak", "checked_at", "exit_ip", "country",
			"anonymity", "listed_count", "source", "added_at", "tags"})
		for _, p := range ps {
			cw.Write([]string{
				strconv.FormatInt(p.ID, 10), p.URL(), p.Scheme, p.Host, strconv.Itoa(p.Port), p.Status,
				itoaOrEmpty(p.LastMS), itoaOrEmpty(p.BestMS), ftoaOrEmpty(p.AvgMS),
				strconv.Itoa(p.OKCount), strconv.Itoa(p.FailCount), strconv.Itoa(p.OKStreak), strconv.Itoa(p.FailStreak),
				timeOrEmpty(p.CheckedAt), p.ExitIP, p.Country, p.Anonymity, strconv.Itoa(p.ListedCount), p.Source,
				timeOrEmpty(p.AddedAt), strings.Join(p.Tags, " "),
			})
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return 0, err
		}
	case FormatJSON:
		if ps == nil {
			ps = []*Proxy{}
		}
		enc := json.NewEncoder(bw)
		enc.SetIndent("", "  ")
		if err := enc.Encode(ps); err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("db: unknown export format %q (want one of %s)", format, strings.Join(ExportFormats, ", "))
	}
	return len(ps), bw.Flush()
}

func itoaOrEmpty(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func ftoaOrEmpty(f float64) string {
	if f == 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}

func timeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
