package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Well-known meta keys.
const (
	MetaFirstImport = "first_import_at"   // unix ms of the automatic first import
	MetaLastImport  = "last_import"       // "<source> <unix ms>"
	MetaLastRebuild = "last_rebuild_at"   // unix ms of the last RebuildStats
	MetaCreatedWith = "created_with"      // binary version that created the file
	MetaLastVacuum  = "last_vacuum_at"    // unix ms
	MetaBuiltinHash = "builtin_list_hash" // hash of the embedded list last imported
)

// GetMeta returns a meta value ("" and false when unset).
func (d *DB) GetMeta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetMeta stores a meta value.
func (d *DB) SetMeta(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// AllMeta returns every meta key/value.
func (d *DB) AllMeta(ctx context.Context) (map[string]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT key, value FROM meta ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Stats is a summary of the database contents.
type Stats struct {
	Proxies      int            `json:"proxies"`
	ByStatus     map[string]int `json:"by_status"`
	ByScheme     map[string]int `json:"by_scheme"`
	BySource     map[string]int `json:"by_source"`
	Measured     int            `json:"measured"` // proxies with at least one fact
	Timed        int            `json:"timed"`    // proxies with a known latency
	Measurements int            `json:"measurements"`
	Reputation   int            `json:"reputation"`
	Listed       int            `json:"listed"` // proxies listed on >= 1 list
	Usage        int            `json:"usage"`
	Tags         int            `json:"tags"`
	ScanRuns     int            `json:"scan_runs"`
	// Latency buckets of alive proxies (last_ms), like the legacy report.
	Latency         LatencyBuckets `json:"latency"`
	BestMS          int            `json:"best_ms,omitempty"` // fastest alive last_ms
	MedianMS        int            `json:"median_ms,omitempty"`
	LastChecked     time.Time      `json:"last_checked,omitzero"`
	LastMeasurement time.Time      `json:"last_measurement,omitzero"`
	SchemaVersion   int            `json:"schema_version"`
}

// LatencyBuckets counts alive proxies by last_ms.
type LatencyBuckets struct {
	Under100 int `json:"under_100ms"`
	To1s     int `json:"0_1s"`
	To5s     int `json:"1_5s"`
	To10s    int `json:"5_10s"`
	Over10s  int `json:"10s_plus"`
	Untimed  int `json:"untimed"`
}

// Stats summarises the database.
func (d *DB) Stats(ctx context.Context) (*Stats, error) {
	s := &Stats{ByStatus: map[string]int{}, ByScheme: map[string]int{}, BySource: map[string]int{}}
	group := func(col string, into map[string]int) error {
		rows, err := d.sql.QueryContext(ctx, `SELECT `+col+`, count(*) FROM proxies GROUP BY 1`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				return err
			}
			into[k] = n
			if col == "status" {
				s.Proxies += n
			}
		}
		return rows.Err()
	}
	if err := group("status", s.ByStatus); err != nil {
		return nil, err
	}
	if err := group("scheme", s.ByScheme); err != nil {
		return nil, err
	}
	if err := group("source", s.BySource); err != nil {
		return nil, err
	}
	var lastChecked, lastFact, best sql.NullInt64
	err := d.sql.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM proxies WHERE checked_at IS NOT NULL),
		(SELECT count(*) FROM proxies WHERE last_ms IS NOT NULL),
		(SELECT count(*) FROM measurements),
		(SELECT count(*) FROM reputation),
		(SELECT count(*) FROM proxies WHERE listed_count > 0),
		(SELECT count(*) FROM usage_log),
		(SELECT count(*) FROM tags),
		(SELECT count(*) FROM scan_runs),
		(SELECT max(checked_at) FROM proxies),
		(SELECT max(ts) FROM measurements),
		(SELECT min(last_ms) FROM proxies WHERE status = 'alive')`).Scan(
		&s.Measured, &s.Timed, &s.Measurements, &s.Reputation, &s.Listed, &s.Usage, &s.Tags, &s.ScanRuns,
		&lastChecked, &lastFact, &best)
	if err != nil {
		return nil, err
	}
	err = d.sql.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE last_ms > 0 AND last_ms < 100),
		count(*) FILTER (WHERE last_ms > 0 AND last_ms <= 1000),
		count(*) FILTER (WHERE last_ms > 1000 AND last_ms <= 5000),
		count(*) FILTER (WHERE last_ms > 5000 AND last_ms <= 10000),
		count(*) FILTER (WHERE last_ms > 10000),
		count(*) FILTER (WHERE last_ms IS NULL)
		FROM proxies WHERE status = 'alive'`).Scan(
		&s.Latency.Under100, &s.Latency.To1s, &s.Latency.To5s, &s.Latency.To10s, &s.Latency.Over10s, &s.Latency.Untimed)
	if err != nil {
		return nil, err
	}
	s.LastChecked, s.LastMeasurement, s.BestMS = fromMS(lastChecked), fromMS(lastFact), int(best.Int64)
	timedAlive := s.ByStatus[StatusAlive] - s.Latency.Untimed
	if timedAlive > 0 {
		var med sql.NullInt64
		if err := d.sql.QueryRowContext(ctx, `SELECT last_ms FROM proxies WHERE status = 'alive' AND last_ms IS NOT NULL
			ORDER BY last_ms LIMIT 1 OFFSET ?`, (timedAlive-1)/2).Scan(&med); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		s.MedianMS = int(med.Int64)
	}
	if s.SchemaVersion, err = d.SchemaVersion(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
