package db

import (
	"context"
	"database/sql"
	"fmt"
)

// Migration is one schema step. Migrations run in Version order, each in its
// own write transaction together with its schema_version row, so a failed
// step leaves the database at the previous version.
//
// Adding a migration (later tasks): append a new entry to `migrations` with
// the next Version number and a short Name. Never edit or reorder an entry
// that has been committed; fix mistakes with a new migration. Up receives
// the transaction; use tx.Exec for DDL and data back-fills. Nothing else
// needs registering: Open applies pending migrations automatically and
// `px db migrate` reports them.
type Migration struct {
	Version int
	Name    string
	Up      func(ctx context.Context, tx *sql.Tx) error
}

// migrations is the ordered list of schema steps.
var migrations = []Migration{
	{Version: 1, Name: "core schema", Up: execAll(schemaV1...)},
}

// execAll returns an Up func that executes each statement in order.
func execAll(stmts ...string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("%w\n%s", err, s)
			}
		}
		return nil
	}
}

// schemaV1 is the core schema from docs/ARCHITECTURE-v4.md, plus:
//   - proxies.username/password: credentials of authenticated proxies (the
//     text cache supports them, so the database must too),
//   - proxies.ms_count/ms_sum: number and sum of timed successful
//     measurements, so avg_ms is exact and RebuildStats reproduces it.
var schemaV1 = []string{
	`CREATE TABLE proxies (
		id                  INTEGER PRIMARY KEY,
		scheme              TEXT    NOT NULL,
		host                TEXT    NOT NULL,
		port                INTEGER NOT NULL,
		username            TEXT    NOT NULL DEFAULT '',
		password            TEXT    NOT NULL DEFAULT '',
		source              TEXT    NOT NULL DEFAULT '',
		added_at            INTEGER NOT NULL,
		last_seen_in_source INTEGER,
		status              TEXT    NOT NULL DEFAULT 'unknown'
		                    CHECK (status IN ('unknown','alive','dead')),
		checked_at          INTEGER,
		last_ms             INTEGER,
		best_ms             INTEGER,
		avg_ms              REAL,
		ms_count            INTEGER NOT NULL DEFAULT 0,
		ms_sum              INTEGER NOT NULL DEFAULT 0,
		ok_count            INTEGER NOT NULL DEFAULT 0,
		fail_count          INTEGER NOT NULL DEFAULT 0,
		ok_streak           INTEGER NOT NULL DEFAULT 0,
		fail_streak         INTEGER NOT NULL DEFAULT 0,
		exit_ip             TEXT,
		country             TEXT,
		asn                 TEXT,
		org                 TEXT,
		anonymity           TEXT    NOT NULL DEFAULT 'unknown'
		                    CHECK (anonymity IN ('unknown','transparent','anonymous','elite')),
		listed_count        INTEGER NOT NULL DEFAULT 0,
		speed_score         REAL,
		clean_score         REAL,
		rank                INTEGER,
		UNIQUE (scheme, host, port)
	)`,
	`CREATE INDEX proxies_status_last_ms ON proxies (status, last_ms)`,
	`CREATE INDEX proxies_rank ON proxies (rank)`,
	`CREATE INDEX proxies_last_ms ON proxies (last_ms)`,
	`CREATE INDEX proxies_host ON proxies (host)`,
	`CREATE INDEX proxies_checked_at ON proxies (checked_at)`,
	`CREATE INDEX proxies_exit_ip ON proxies (exit_ip)`,

	`CREATE TABLE measurements (
		id        INTEGER PRIMARY KEY,
		proxy_id  INTEGER NOT NULL REFERENCES proxies(id) ON DELETE CASCADE,
		ts        INTEGER NOT NULL,
		kind      TEXT    NOT NULL CHECK (kind IN ('probe','preflight','route','scan')),
		probe     TEXT    NOT NULL DEFAULT '',
		ok        INTEGER NOT NULL,
		ms        INTEGER,
		err_class TEXT,
		exit_ip   TEXT,
		detail    TEXT
	)`,
	`CREATE INDEX measurements_proxy_ts ON measurements (proxy_id, ts)`,
	`CREATE INDEX measurements_ts ON measurements (ts)`,

	`CREATE TABLE reputation (
		id         INTEGER PRIMARY KEY,
		ip         TEXT    NOT NULL,
		list_name  TEXT    NOT NULL,
		list_kind  TEXT    NOT NULL
		           CHECK (list_kind IN ('dnsbl','ipset','proxylist','torexit','datacenter','api')),
		listed     INTEGER NOT NULL,
		detail     TEXT,
		checked_at INTEGER NOT NULL,
		UNIQUE (ip, list_name)
	)`,
	`CREATE INDEX reputation_ip ON reputation (ip, listed)`,

	`CREATE TABLE usage_log (
		id           INTEGER PRIMARY KEY,
		ts_start     INTEGER NOT NULL,
		ts_end       INTEGER,
		proxy_id     INTEGER REFERENCES proxies(id) ON DELETE SET NULL,
		proxy_url    TEXT    NOT NULL DEFAULT '',
		mode         TEXT    NOT NULL CHECK (mode IN ('route','shell','relay')),
		argv         TEXT,
		cmd          TEXT    NOT NULL DEFAULT '',
		cwd          TEXT,
		tty          TEXT,
		pid          INTEGER,
		ppid         INTEGER,
		session_id   TEXT,
		preflight_ok INTEGER,
		preflight_ms INTEGER,
		exit_code    INTEGER,
		duration_ms  INTEGER,
		err_class    TEXT
	)`,
	`CREATE INDEX usage_log_proxy_ts ON usage_log (proxy_id, ts_start)`,
	`CREATE INDEX usage_log_cmd_ts ON usage_log (cmd, ts_start)`,
	`CREATE INDEX usage_log_ts ON usage_log (ts_start)`,
	`CREATE INDEX usage_log_session ON usage_log (session_id)`,

	`CREATE TABLE tags (
		proxy_id INTEGER NOT NULL REFERENCES proxies(id) ON DELETE CASCADE,
		tag      TEXT    NOT NULL,
		scope    TEXT    NOT NULL DEFAULT '',
		since    INTEGER NOT NULL,
		reason   TEXT    NOT NULL DEFAULT '',
		PRIMARY KEY (proxy_id, tag, scope)
	)`,
	`CREATE INDEX tags_tag ON tags (tag, scope)`,

	`CREATE TABLE scan_runs (
		id          INTEGER PRIMARY KEY,
		mode        TEXT    NOT NULL,
		started_at  INTEGER NOT NULL,
		finished_at INTEGER,
		total       INTEGER NOT NULL DEFAULT 0,
		done        INTEGER NOT NULL DEFAULT 0,
		cursor      TEXT    NOT NULL DEFAULT '',
		opts        TEXT,
		state       TEXT    NOT NULL DEFAULT 'running'
		            CHECK (state IN ('running','paused','done','failed'))
	)`,
	`CREATE INDEX scan_runs_mode_state ON scan_runs (mode, state)`,

	`CREATE TABLE meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
}

// MigrationInfo describes one known migration and whether it is applied.
type MigrationInfo struct {
	Version   int    `json:"version"`
	Name      string `json:"name"`
	Applied   bool   `json:"applied"`
	AppliedAt int64  `json:"applied_at,omitempty"` // unix ms
}

// LatestVersion is the highest schema version this binary knows.
func LatestVersion() int { return migrations[len(migrations)-1].Version }

const createSchemaVersion = `CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL,
	applied_at INTEGER NOT NULL
)`

// SchemaVersion returns the highest applied migration (0 for a new file).
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var exists int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&exists); err != nil {
		return 0, fmt.Errorf("db: schema version: %w", err)
	}
	if exists == 0 {
		return 0, nil
	}
	var v sql.NullInt64
	if err := d.sql.QueryRowContext(ctx, `SELECT max(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("db: schema version: %w", err)
	}
	return int(v.Int64), nil
}

// Migrations lists every known migration with its applied state.
func (d *DB) Migrations(ctx context.Context) ([]MigrationInfo, error) {
	cur, err := d.SchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	applied := map[int]int64{}
	if cur > 0 {
		rows, err := d.sql.QueryContext(ctx, `SELECT version, applied_at FROM schema_version`)
		if err != nil {
			return nil, fmt.Errorf("db: migrations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var v int
			var at int64
			if err := rows.Scan(&v, &at); err != nil {
				return nil, err
			}
			applied[v] = at
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := make([]MigrationInfo, 0, len(migrations))
	for _, m := range migrations {
		at, ok := applied[m.Version]
		out = append(out, MigrationInfo{Version: m.Version, Name: m.Name, Applied: ok, AppliedAt: at})
	}
	return out, nil
}

// Migrate applies every pending migration and returns how many ran. It is
// idempotent and safe against a concurrent first run in another process:
// the current version is re-read inside each step's immediate transaction.
func (d *DB) Migrate(ctx context.Context) (int, error) {
	// Fast path, no write lock: an up-to-date database (every open after
	// the first) must not contend with a running bulk writer.
	if cur, err := d.SchemaVersion(ctx); err == nil && cur >= LatestVersion() {
		if cur > LatestVersion() {
			return 0, fmt.Errorf("db: %s has schema version %d, newer than this binary (%d); upgrade SilkRouter",
				d.path, cur, LatestVersion())
		}
		return 0, nil
	}
	if _, err := d.sql.ExecContext(ctx, createSchemaVersion); err != nil {
		return 0, fmt.Errorf("db: migrate: %w", err)
	}
	n := 0
	for _, m := range migrations {
		ran := false
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			var cur sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT max(version) FROM schema_version`).Scan(&cur); err != nil {
				return err
			}
			if int(cur.Int64) >= m.Version {
				return nil
			}
			if int(cur.Int64) != m.Version-1 {
				return fmt.Errorf("schema is at version %d, cannot apply %d", cur.Int64, m.Version)
			}
			if err := m.Up(ctx, tx); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO schema_version(version, name, applied_at) VALUES (?, ?, ?)`,
				m.Version, m.Name, nowFunc().UnixMilli()); err != nil {
				return err
			}
			ran = true
			return nil
		})
		if err != nil {
			return n, fmt.Errorf("db: migration %d (%s): %w", m.Version, m.Name, err)
		}
		if ran {
			n++
		}
	}
	cur, err := d.SchemaVersion(ctx)
	if err != nil {
		return n, err
	}
	if cur > LatestVersion() {
		return n, fmt.Errorf("db: %s has schema version %d, newer than this binary (%d); upgrade SilkRouter",
			d.path, cur, LatestVersion())
	}
	return n, nil
}
