package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

const scanRunCols = `id, mode, started_at, finished_at, total, done, cursor, opts, state`

func scanScanRun(s scanner) (*ScanRun, error) {
	var r ScanRun
	var started int64
	var finished sql.NullInt64
	var opts sql.NullString
	if err := s.Scan(&r.ID, &r.Mode, &started, &finished, &r.Total, &r.Done, &r.Cursor, &opts, &r.State); err != nil {
		return nil, err
	}
	r.StartedAt, r.FinishedAt, r.Opts = time.UnixMilli(started).UTC(), fromMS(finished), opts.String
	return &r, nil
}

// CreateScanRun starts a scan run (state "running" unless set) and returns
// its id.
func (d *DB) CreateScanRun(ctx context.Context, r ScanRun) (int64, error) {
	if r.Mode == "" {
		return 0, fmt.Errorf("db: scan run needs a mode")
	}
	if r.State == "" {
		r.State = ScanRunning
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = nowFunc()
	}
	res, err := d.sql.ExecContext(ctx, `INSERT INTO scan_runs (mode, started_at, total, done, cursor, opts, state)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, r.Mode, r.StartedAt.UnixMilli(), r.Total, r.Done, r.Cursor, nullStr(r.Opts), r.State)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateScanRun stores progress: done, cursor and state (a terminal state —
// done or failed — also sets finished_at). Total is updated when > 0.
func (d *DB) UpdateScanRun(ctx context.Context, id int64, done int, cursor, state string, total int) error {
	if state != "" && !slices.Contains([]string{ScanRunning, ScanPaused, ScanDone, ScanFailed}, state) {
		return fmt.Errorf("db: scan state %q", state)
	}
	var finished any
	if state == ScanDone || state == ScanFailed {
		finished = nowFunc().UnixMilli()
	}
	r, err := d.sql.ExecContext(ctx, `UPDATE scan_runs SET done = ?, cursor = ?,
		state = coalesce(nullif(?, ''), state),
		total = CASE WHEN ? > 0 THEN ? ELSE total END,
		finished_at = coalesce(?, finished_at)
		WHERE id = ?`, done, cursor, state, total, total, finished, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("scan run #%d: %w", id, ErrNotFound)
	}
	return nil
}

// GetScanRun loads one scan run.
func (d *DB) GetScanRun(ctx context.Context, id int64) (*ScanRun, error) {
	r, err := scanScanRun(d.sql.QueryRowContext(ctx, `SELECT `+scanRunCols+` FROM scan_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan run #%d: %w", id, ErrNotFound)
	}
	return r, err
}

// ResumableScanRun returns the newest unfinished (running or paused) run of
// mode, or ErrNotFound. A "running" run found here was interrupted (its
// process died); the caller resumes it from Cursor.
func (d *DB) ResumableScanRun(ctx context.Context, mode string) (*ScanRun, error) {
	r, err := scanScanRun(d.sql.QueryRowContext(ctx, `SELECT `+scanRunCols+` FROM scan_runs
		WHERE mode = ? AND state IN ('running', 'paused') ORDER BY id DESC LIMIT 1`, mode))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("resumable %s scan: %w", mode, ErrNotFound)
	}
	return r, err
}

// ListScanRuns returns the newest runs first (limit <= 0 = all).
func (d *DB) ListScanRuns(ctx context.Context, limit int) ([]*ScanRun, error) {
	q, args := `SELECT `+scanRunCols+` FROM scan_runs ORDER BY id DESC`, []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ScanRun
	for rows.Next() {
		r, err := scanScanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
