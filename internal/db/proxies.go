package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
)

// ErrNotFound is returned when a proxy (or other row) does not exist.
var ErrNotFound = errors.New("not found")

// proxyCols is the SELECT list matching scanProxy.
const proxyCols = `p.id, p.scheme, p.host, p.port, p.username, p.password, p.source,
	p.added_at, p.last_seen_in_source, p.status, p.checked_at, p.last_ms, p.best_ms,
	p.avg_ms, p.ms_count, p.ms_sum, p.ok_count, p.fail_count, p.ok_streak, p.fail_streak,
	p.exit_ip, p.country, p.asn, p.org, p.anonymity, p.listed_count, p.speed_score,
	p.clean_score, p.rank`

type scanner interface{ Scan(dest ...any) error }

func scanProxy(s scanner, extra ...any) (*Proxy, error) {
	var p Proxy
	var added, seen, checked, lastMS, bestMS, rank sql.NullInt64
	var avg, speed, clean sql.NullFloat64
	var exitIP, country, asn, org sql.NullString
	dest := []any{&p.ID, &p.Scheme, &p.Host, &p.Port, &p.Username, &p.Password, &p.Source,
		&added, &seen, &p.Status, &checked, &lastMS, &bestMS,
		&avg, &p.MSCount, &p.MSSum, &p.OKCount, &p.FailCount, &p.OKStreak, &p.FailStreak,
		&exitIP, &country, &asn, &org, &p.Anonymity, &p.ListedCount, &speed,
		&clean, &rank}
	if err := s.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	p.AddedAt, p.LastSeenInSource, p.CheckedAt = fromMS(added), fromMS(seen), fromMS(checked)
	p.LastMS, p.BestMS = int(lastMS.Int64), int(bestMS.Int64)
	p.AvgMS = avg.Float64
	p.ExitIP, p.Country, p.ASN, p.Org = exitIP.String, country.String, asn.String, org.String
	if speed.Valid {
		p.SpeedScore = &speed.Float64
	}
	if clean.Valid {
		p.CleanScore = &clean.Float64
	}
	if rank.Valid {
		r := int(rank.Int64)
		p.Rank = &r
	}
	return &p, nil
}

const upsertProxySQL = `INSERT INTO proxies (scheme, host, port, username, password, source, added_at, last_seen_in_source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (scheme, host, port) DO UPDATE SET
	username = CASE WHEN excluded.username <> '' THEN excluded.username ELSE proxies.username END,
	password = CASE WHEN excluded.username <> '' THEN excluded.password ELSE proxies.password END,
	source   = CASE WHEN proxies.source = '' THEN excluded.source ELSE proxies.source END,
	last_seen_in_source = coalesce(excluded.last_seen_in_source, proxies.last_seen_in_source)
RETURNING id`

// upsertProxyTx inserts or refreshes a proxy and returns its id. seen marks
// last_seen_in_source (zero leaves it alone). The first non-empty source
// sticks.
func upsertProxyTx(ctx context.Context, stmt *sql.Stmt, a Addr, source string, now, seen time.Time) (int64, error) {
	if a.Scheme == "" || a.Host == "" || a.Port <= 0 {
		return 0, fmt.Errorf("db: invalid proxy %q", a.String())
	}
	var id int64
	err := stmt.QueryRowContext(ctx, a.Scheme, strings.ToLower(a.Host), a.Port, a.Username, a.Password,
		source, now.UnixMilli(), ms(seen)).Scan(&id)
	return id, err
}

// maxProxyID is the highest proxy id (rows added later have larger ids).
func maxProxyID(ctx context.Context, tx *sql.Tx) (int64, error) {
	var n sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT max(id) FROM proxies`).Scan(&n)
	return n.Int64, err
}

// UpsertProxy inserts a proxy or refreshes an existing one (same scheme,
// host and port) and returns its id.
func (d *DB) UpsertProxy(ctx context.Context, a Addr, source string) (int64, error) {
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, upsertProxySQL)
		if err != nil {
			return err
		}
		defer stmt.Close()
		now := nowFunc()
		id, err = upsertProxyTx(ctx, stmt, a, source, now, now)
		return err
	})
	return id, err
}

// ImportResult reports a bulk import.
type ImportResult struct {
	Total        int           `json:"total"`        // proxies offered
	Added        int           `json:"added"`        // new rows
	Existing     int           `json:"existing"`     // already known
	Measurements int           `json:"measurements"` // latency facts recorded
	Elapsed      time.Duration `json:"elapsed_ns"`
}

// ImportOptions tune ImportProxies.
type ImportOptions struct {
	// Source labels new rows ("builtin", "url:host", "file:name", ...).
	Source string
	// Latencies records a probe measurement (ok, ms) for every entry that
	// carries a "[Nms]" latency, under Probe (default "legacy").
	Latencies bool
	// AllAlive records an ok measurement for every entry, timed or not (a
	// legacy text cache only ever held working proxies). Implies Latencies.
	AllAlive bool
	Probe    string
}

// ImportProxies upserts ps in one transaction (fast: ~13k rows in well under
// a second) and optionally records their cached latencies.
func (d *DB) ImportProxies(ctx context.Context, ps []proxy.Proxy, opts ImportOptions) (ImportResult, error) {
	start := time.Now()
	res := ImportResult{Total: len(ps)}
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		r, err := importTx(ctx, tx, ps, opts)
		res = r
		return err
	})
	res.Elapsed = time.Since(start)
	return res, err
}

func importTx(ctx context.Context, tx *sql.Tx, ps []proxy.Proxy, opts ImportOptions) (ImportResult, error) {
	res := ImportResult{Total: len(ps)}
	stmt, err := tx.PrepareContext(ctx, upsertProxySQL)
	if err != nil {
		return res, err
	}
	defer stmt.Close()
	probeName := opts.Probe
	if probeName == "" {
		probeName = ProbeLegacy
	}
	before, err := maxProxyID(ctx, tx)
	if err != nil {
		return res, err
	}
	now := nowFunc()
	var facts []Measurement
	seen := make(map[int64]bool, len(ps))
	for _, p := range ps {
		id, err := upsertProxyTx(ctx, stmt, AddrOf(p), opts.Source, now, now)
		if err != nil {
			return res, fmt.Errorf("db: import %s: %w", p.Key(), err)
		}
		if seen[id] {
			continue // duplicate entry in the input
		}
		seen[id] = true
		if id > before {
			res.Added++
		} else {
			res.Existing++
		}
		if opts.AllAlive || (opts.Latencies && p.LatencyMS > 0) {
			facts = append(facts, Measurement{ProxyID: id, TS: now, Kind: KindProbe, Probe: probeName, OK: true, MS: p.LatencyMS})
		}
	}
	if len(facts) > 0 {
		if err := recordTx(ctx, tx, facts); err != nil {
			return res, err
		}
		res.Measurements = len(facts)
	}
	return res, nil
}

// GetProxy loads one proxy by id.
func (d *DB) GetProxy(ctx context.Context, id int64) (*Proxy, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+proxyCols+` FROM proxies p WHERE p.id = ?`, id)
	p, err := scanProxy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("proxy #%d: %w", id, ErrNotFound)
	}
	return p, err
}

// GetProxyByAddr loads one proxy by scheme, host and port.
func (d *DB) GetProxyByAddr(ctx context.Context, a Addr) (*Proxy, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+proxyCols+` FROM proxies p
		WHERE p.scheme = ? AND p.host = ? AND p.port = ?`, a.Scheme, strings.ToLower(a.Host), a.Port)
	p, err := scanProxy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("proxy %s: %w", a, ErrNotFound)
	}
	return p, err
}

// GetProxyByURL loads one proxy by URL ("socks5://1.2.3.4:1080", with or
// without a "[Nms]" suffix).
func (d *DB) GetProxyByURL(ctx context.Context, s string) (*Proxy, error) {
	a, err := ParseAddr(s)
	if err != nil {
		return nil, err
	}
	return d.GetProxyByAddr(ctx, a)
}

// FindProxy resolves a user-supplied reference: a numeric id ("42" or
// "#42"), a proxy URL, or a bare host:port / host that matches exactly one
// proxy.
func (d *DB) FindProxy(ctx context.Context, ref string) (*Proxy, error) {
	ref = strings.TrimSpace(ref)
	if n, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err == nil {
		return d.GetProxy(ctx, n)
	}
	if strings.Contains(ref, "://") {
		return d.GetProxyByURL(ctx, ref)
	}
	host, port := ref, 0
	if h, p, ok := strings.Cut(ref, ":"); ok {
		if n, err := strconv.Atoi(p); err == nil {
			host, port = h, n
		}
	}
	q := `SELECT ` + proxyCols + ` FROM proxies p WHERE p.host = ?`
	args := []any{strings.ToLower(host)}
	if port > 0 {
		q += ` AND p.port = ?`
		args = append(args, port)
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY p.id LIMIT 10`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []*Proxy
	for rows.Next() {
		p, err := scanProxy(rows)
		if err != nil {
			return nil, err
		}
		found = append(found, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("proxy %q: %w", ref, ErrNotFound)
	case 1:
		return found[0], nil
	default:
		var keys []string
		for _, p := range found {
			keys = append(keys, p.Key())
		}
		return nil, fmt.Errorf("proxy %q is ambiguous: %s", ref, strings.Join(keys, ", "))
	}
}

// DeleteProxy removes a proxy and (by cascade) its measurements and tags.
func (d *DB) DeleteProxy(ctx context.Context, id int64) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM proxies WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return fmt.Errorf("proxy #%d: %w", id, ErrNotFound)
		}
		return nil
	})
}

// CountProxies counts rows matching f (Limit/Offset/Sort are ignored).
func (d *DB) CountProxies(ctx context.Context, f Filter) (int, error) {
	where, args := f.where()
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM proxies p`+where, args...).Scan(&n)
	return n, err
}

// ListProxies returns the proxies matching f, sorted and paginated. It is
// the one query route, the CLI and the TUI share.
func (d *DB) ListProxies(ctx context.Context, f Filter) ([]*Proxy, error) {
	where, args := f.where()
	q := `SELECT ` + proxyCols
	if f.WithTags {
		q += `, (SELECT group_concat(CASE WHEN t.scope = '' THEN t.tag ELSE t.tag || ':' || t.scope END, ' ')
			FROM tags t WHERE t.proxy_id = p.id)`
	}
	q += ` FROM proxies p` + where
	order, err := f.orderBy()
	if err != nil {
		return nil, err
	}
	q += order
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
		if f.Offset > 0 {
			q += ` OFFSET ?`
			args = append(args, f.Offset)
		}
	} else if f.Offset > 0 {
		q += ` LIMIT -1 OFFSET ?`
		args = append(args, f.Offset)
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list proxies: %w", err)
	}
	defer rows.Close()
	var out []*Proxy
	for rows.Next() {
		var tags sql.NullString
		var extra []any
		if f.WithTags {
			extra = append(extra, &tags)
		}
		p, err := scanProxy(rows, extra...)
		if err != nil {
			return nil, err
		}
		if tags.String != "" {
			p.Tags = strings.Fields(tags.String)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetScore is one row for SetScores.
type SetScore struct {
	ProxyID    int64
	SpeedScore *float64
	CleanScore *float64
	Rank       *int
}

// SetScores writes speed_score, clean_score and rank (the scan engine's
// ranking output) in one transaction. Nil fields are stored as NULL.
func (d *DB) SetScores(ctx context.Context, scores []SetScore) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `UPDATE proxies SET speed_score = ?, clean_score = ?, rank = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, s := range scores {
			var rank any
			if s.Rank != nil {
				rank = *s.Rank
			}
			var sp, cl any
			if s.SpeedScore != nil {
				sp = *s.SpeedScore
			}
			if s.CleanScore != nil {
				cl = *s.CleanScore
			}
			if _, err := stmt.ExecContext(ctx, sp, cl, rank, s.ProxyID); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetGeo stores what was learned about a proxy's exit (country, ASN, org,
// anonymity). Empty strings leave the column unchanged.
func (d *DB) SetGeo(ctx context.Context, id int64, country, asn, org, anonymity string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE proxies SET
		country   = coalesce(?, country),
		asn       = coalesce(?, asn),
		org       = coalesce(?, org),
		anonymity = coalesce(?, anonymity)
		WHERE id = ?`, nullStr(country), nullStr(asn), nullStr(org), nullStr(anonymity), id)
	return err
}
