package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// StartUsage inserts a usage_log row for a command about to run and returns
// its id. Cmd defaults to the basename of Argv[0]; TSStart defaults to now.
// FinishUsage completes the row when the command exits.
func (d *DB) StartUsage(ctx context.Context, u Usage) (int64, error) {
	if u.Mode == "" {
		u.Mode = ModeRoute
	}
	if !slices.Contains([]string{ModeRoute, ModeShell, ModeRelay}, u.Mode) {
		return 0, fmt.Errorf("db: usage mode %q", u.Mode)
	}
	if u.TSStart.IsZero() {
		u.TSStart = nowFunc()
	}
	if u.Cmd == "" && len(u.Argv) > 0 {
		u.Cmd = filepath.Base(strings.ReplaceAll(u.Argv[0], `\`, "/"))
	}
	argv, err := json.Marshal(u.Argv)
	if err != nil {
		return 0, err
	}
	var proxyID any
	if u.ProxyID > 0 {
		proxyID = u.ProxyID
	}
	var pok, pms any
	if u.PreflightOK != nil {
		pok = *u.PreflightOK
	}
	if u.PreflightMS != nil {
		pms = *u.PreflightMS
	}
	var id int64
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `INSERT INTO usage_log (ts_start, proxy_id, proxy_url, mode, argv, cmd,
			cwd, tty, pid, ppid, session_id, preflight_ok, preflight_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			u.TSStart.UnixMilli(), proxyID, u.ProxyURL, u.Mode, string(argv), u.Cmd,
			nullStr(u.Cwd), nullStr(u.TTY), nullInt(u.PID), nullInt(u.PPID), nullStr(u.SessionID), pok, pms)
		if err != nil {
			return err
		}
		id, err = r.LastInsertId()
		return err
	})
	return id, err
}

// FinishUsage records the end of a usage row: exit code, duration (from
// ts_start to end) and an optional err_class ("exit_nonzero" is filled in
// automatically for a non-zero exit when errClass is empty). end zero = now.
func (d *DB) FinishUsage(ctx context.Context, id int64, exitCode int, errClass string, end time.Time) error {
	if end.IsZero() {
		end = nowFunc()
	}
	if errClass == "" && exitCode != 0 {
		errClass = ErrExitNonzero
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `UPDATE usage_log SET ts_end = ?1, exit_code = ?2,
			duration_ms = max(0, ?1 - ts_start), err_class = ?3 WHERE id = ?4`,
			end.UnixMilli(), exitCode, nullStr(errClass), id)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return fmt.Errorf("usage #%d: %w", id, ErrNotFound)
		}
		return nil
	})
}

// SetUsagePreflight records the preflight outcome of a started usage row.
func (d *DB) SetUsagePreflight(ctx context.Context, id int64, ok bool, ms int) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE usage_log SET preflight_ok = ?, preflight_ms = ? WHERE id = ?`,
		ok, nullInt(ms), id)
	return err
}

// UsageFilter selects usage_log rows (newest first).
type UsageFilter struct {
	ProxyID   int64
	Cmd       string
	SessionID string
	Since     time.Time
	Limit     int
}

// ListUsage returns usage rows matching f, newest first.
func (d *DB) ListUsage(ctx context.Context, f UsageFilter) ([]Usage, error) {
	var conds []string
	var args []any
	if f.ProxyID > 0 {
		conds = append(conds, "proxy_id = ?")
		args = append(args, f.ProxyID)
	}
	if f.Cmd != "" {
		conds = append(conds, "cmd = ?")
		args = append(args, f.Cmd)
	}
	if f.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "ts_start >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	q := `SELECT id, ts_start, ts_end, proxy_id, proxy_url, mode, argv, cmd, cwd, tty, pid, ppid,
		session_id, preflight_ok, preflight_ms, exit_code, duration_ms, err_class FROM usage_log`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY ts_start DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Usage
	for rows.Next() {
		var u Usage
		var start int64
		var end, pid, ppid, proxyID, pok, pms, code, dur sql.NullInt64
		var argv, cwd, tty, sess, ec sql.NullString
		if err := rows.Scan(&u.ID, &start, &end, &proxyID, &u.ProxyURL, &u.Mode, &argv, &u.Cmd, &cwd, &tty,
			&pid, &ppid, &sess, &pok, &pms, &code, &dur, &ec); err != nil {
			return nil, err
		}
		u.TSStart, u.TSEnd = time.UnixMilli(start).UTC(), fromMS(end)
		u.ProxyID, u.PID, u.PPID = proxyID.Int64, int(pid.Int64), int(ppid.Int64)
		u.Cwd, u.TTY, u.SessionID, u.ErrClass = cwd.String, tty.String, sess.String, ec.String
		u.DurationMS = dur.Int64
		if argv.Valid {
			_ = json.Unmarshal([]byte(argv.String), &u.Argv)
		}
		if pok.Valid {
			b := pok.Int64 != 0
			u.PreflightOK = &b
		}
		if pms.Valid {
			n := int(pms.Int64)
			u.PreflightMS = &n
		}
		if code.Valid {
			n := int(code.Int64)
			u.ExitCode = &n
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UsageCount is the number of usage rows for a proxy (0 = all rows).
func (d *DB) UsageCount(ctx context.Context, proxyID int64) (int, error) {
	q, args := `SELECT count(*) FROM usage_log`, []any{}
	if proxyID > 0 {
		q += ` WHERE proxy_id = ?`
		args = append(args, proxyID)
	}
	var n int
	err := d.sql.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}
