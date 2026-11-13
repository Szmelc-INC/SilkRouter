// Package db is SilkRouter's SQLite store: every known proxy and everything
// ever learned about it (measurements, reputation, usage, tags, scan runs).
//
// The driver is modernc.org/sqlite (pure Go), so CGO_ENABLED=0 builds and
// cross-compiles keep working. Every connection runs with WAL journaling,
// a busy timeout, foreign keys and synchronous=NORMAL; write transactions
// start with BEGIN IMMEDIATE so concurrent processes queue on the busy
// timeout instead of failing on a read->write lock upgrade.
//
// Fact tables (measurements, reputation, usage_log) are the source of truth.
// The summary columns on proxies are denormalised for fast sorting and are
// updated in the same transaction as the fact insert (RecordMeasurement,
// Writer); RebuildStats recomputes them from the facts.
//
// Timestamps are stored as INTEGER unix milliseconds (UTC).
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DefaultBusyTimeout is how long a connection waits for a lock held by
// another connection or process before giving up with SQLITE_BUSY.
const DefaultBusyTimeout = 5 * time.Second

// DB is an open SilkRouter database.
type DB struct {
	sql  *sql.DB
	path string
}

// Options tune Open.
type Options struct {
	// BusyTimeout overrides DefaultBusyTimeout (0 keeps the default).
	BusyTimeout time.Duration
	// NoMigrate skips applying pending migrations (px db migrate --status).
	NoMigrate bool
	// ReadOnly opens the file read-only (it must already exist; no
	// migrations are applied).
	ReadOnly bool
}

// Open opens (creating if needed) the database at path and applies pending
// migrations.
func Open(path string) (*DB, error) { return OpenWith(path, Options{}) }

// OpenWith is Open with options.
func OpenWith(path string, opts Options) (*DB, error) {
	if path == "" {
		return nil, errors.New("db: empty path")
	}
	if !opts.ReadOnly {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("db: %w", err)
			}
		}
	} else if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	bt := opts.BusyTimeout
	if bt <= 0 {
		bt = DefaultBusyTimeout
	}
	sdb, err := sql.Open("sqlite", dsn(path, bt, opts.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	// Readers run concurrently under WAL; writers serialise on the file lock.
	sdb.SetMaxOpenConns(8)
	sdb.SetMaxIdleConns(4)
	sdb.SetConnMaxIdleTime(time.Minute)
	d := &DB{sql: sdb, path: path}
	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	if created {
		_ = os.Chmod(path, 0o600) // proxies may carry credentials
	}
	if !opts.NoMigrate && !opts.ReadOnly {
		if _, err := d.Migrate(context.Background()); err != nil {
			sdb.Close()
			return nil, err
		}
	}
	return d, nil
}

// dsn builds the modernc.org/sqlite connection string. Pragmas live in the
// DSN so every pooled connection gets them, not just the first one.
func dsn(path string, busy time.Duration, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("_txlock", "immediate")
	}
	// A plain path (no "file:" prefix): the driver strips the query and
	// opens the file name verbatim, which also works for Windows paths.
	return path + "?" + q.Encode()
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// Path is the database file.
func (d *DB) Path() string { return d.path }

// SQL exposes the underlying *sql.DB for queries this package does not
// wrap (reports, ad-hoc tooling). Writes should go through the typed API.
func (d *DB) SQL() *sql.DB { return d.sql }

// Tx runs fn inside one write transaction (BEGIN IMMEDIATE), committing
// when fn returns nil and rolling back otherwise.
func (d *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// Vacuum checkpoints the WAL and rebuilds the file, reclaiming free pages.
func (d *DB) Vacuum(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("db: checkpoint: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("db: vacuum: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("db: checkpoint: %w", err)
	}
	return nil
}

// Backup writes a consistent copy of the database (WAL content included)
// to dst, which must not exist.
func (d *DB) Backup(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("db: backup %s: file exists", dst)
	}
	if _, err := d.sql.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("db: backup %s: %w", dst, err)
	}
	return nil
}

// Size is the on-disk size of the database file plus its -wal file.
func Size(path string) (dbBytes, walBytes int64) {
	if fi, err := os.Stat(path); err == nil {
		dbBytes = fi.Size()
	}
	if fi, err := os.Stat(path + "-wal"); err == nil {
		walBytes = fi.Size()
	}
	return
}

// Remove deletes the database file and its -wal/-shm companions. The
// database must be closed first (Windows cannot delete open files).
func Remove(path string) error {
	var errs []error
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---- time helpers ----------------------------------------------------------

// ms converts t to unix milliseconds; the zero time becomes NULL.
func ms(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// fromMS converts nullable unix milliseconds to a time (zero when NULL).
func fromMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64).UTC()
}

// nowFunc is the clock (tests may replace it).
var nowFunc = func() time.Time { return time.Now().UTC() }

// nullStr stores "" as NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt stores 0 as NULL.
func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
