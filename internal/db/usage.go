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

// Usage modes, as a list.
var Modes = []string{ModeRoute, ModeShell, ModeRelay}

func normalizeUsage(u *Usage) error {
	if u.Mode == "" {
		u.Mode = ModeRoute
	}
	if !slices.Contains(Modes, u.Mode) {
		return fmt.Errorf("db: usage mode %q", u.Mode)
	}
	if u.TSStart.IsZero() {
		u.TSStart = nowFunc()
	}
	if u.Cmd == "" && len(u.Argv) > 0 {
		u.Cmd = filepath.Base(strings.ReplaceAll(u.Argv[0], `\`, "/"))
	}
	return nil
}

const insertUsageSQL = `INSERT INTO usage_log (ts_start, ts_end, proxy_id, proxy_url, mode, argv, cmd,
	cwd, tty, pid, ppid, session_id, preflight_ok, preflight_ms, exit_code, duration_ms, err_class, ms)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func insertUsageTx(ctx context.Context, tx *sql.Tx, u *Usage) (int64, error) {
	argv, err := json.Marshal(u.Argv)
	if err != nil {
		return 0, err
	}
	var proxyID any
	if u.ProxyID > 0 {
		proxyID = u.ProxyID
	}
	var pok, pms, code, dur any
	if u.PreflightOK != nil {
		pok = *u.PreflightOK
	}
	if u.PreflightMS != nil {
		pms = *u.PreflightMS
	}
	if u.ExitCode != nil {
		code = *u.ExitCode
		if u.TSEnd.IsZero() {
			u.TSEnd = nowFunc()
		}
		dur = max(int64(0), u.TSEnd.UnixMilli()-u.TSStart.UnixMilli())
		if u.ErrClass == "" && *u.ExitCode != 0 {
			u.ErrClass = ErrExitNonzero
		}
	}
	r, err := tx.ExecContext(ctx, insertUsageSQL,
		u.TSStart.UnixMilli(), ms(u.TSEnd), proxyID, u.ProxyURL, u.Mode, string(argv), u.Cmd,
		nullStr(u.Cwd), nullStr(u.TTY), nullInt(u.PID), nullInt(u.PPID), nullStr(u.SessionID), pok, pms,
		code, dur, nullStr(u.ErrClass), nullInt(u.MS))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// usageCountersSQL adds one finished use to its proxy's counters.
const usageCountersSQL = `UPDATE proxies SET
	use_count     = use_count + 1,
	use_ok        = use_ok + (?1 = 0),
	use_fail      = use_fail + (?1 <> 0),
	use_proxy_err = use_proxy_err + ?2,
	last_used_at  = max(coalesce(last_used_at, 0), ?3),
	last_cmd      = CASE WHEN ?3 >= coalesce(last_used_at, 0) THEN ?4 ELSE last_cmd END
	WHERE id = ?5`

func bumpCountersTx(ctx context.Context, tx *sql.Tx, proxyID int64, exitCode int, errClass string, end time.Time, cmd string) error {
	if proxyID <= 0 {
		return nil
	}
	perr := 0
	if IsProxyErr(errClass) {
		perr = 1
	}
	_, err := tx.ExecContext(ctx, usageCountersSQL, exitCode, perr, end.UnixMilli(), cmd, proxyID)
	return err
}

// StartUsage inserts a usage_log row for a command about to run and returns
// its id. Cmd defaults to the basename of Argv[0]; TSStart defaults to now.
// FinishUsage completes the row when the command exits.
func (d *DB) StartUsage(ctx context.Context, u Usage) (int64, error) {
	if err := normalizeUsage(&u); err != nil {
		return 0, err
	}
	u.ExitCode, u.TSEnd = nil, time.Time{}
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = insertUsageTx(ctx, tx, &u)
		return err
	})
	return id, err
}

// RecordUsage inserts a usage row in one go (a command that already ran,
// e.g. logged by the shell hooks). When ExitCode is set the proxy's usage
// counters are updated in the same transaction.
func (d *DB) RecordUsage(ctx context.Context, u Usage) (int64, error) {
	if err := normalizeUsage(&u); err != nil {
		return 0, err
	}
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = insertUsageTx(ctx, tx, &u); err != nil {
			return err
		}
		if u.ExitCode != nil {
			return bumpCountersTx(ctx, tx, u.ProxyID, *u.ExitCode, u.ErrClass, u.TSEnd, u.Cmd)
		}
		return nil
	})
	return id, err
}

// UsageEnd is how a command ended (FinishUsageWith).
type UsageEnd struct {
	ExitCode int
	// ErrClass classifies a failure; "exit_nonzero" is filled in for a
	// non-zero exit when empty.
	ErrClass string
	End      time.Time // zero = now
	MS       int       // proxy latency measured while it ran (0 = none)
}

// FinishUsage records the end of a usage row: exit code, duration (from
// ts_start to end) and an optional err_class ("exit_nonzero" is filled in
// automatically for a non-zero exit when errClass is empty). end zero = now.
func (d *DB) FinishUsage(ctx context.Context, id int64, exitCode int, errClass string, end time.Time) error {
	return d.FinishUsageWith(ctx, id, UsageEnd{ExitCode: exitCode, ErrClass: errClass, End: end})
}

// FinishUsageWith is FinishUsage plus the latency measured during the
// command; it also updates the proxy's usage counters (use_count, use_ok,
// use_fail, use_proxy_err, last_used_at, last_cmd) in the same transaction.
func (d *DB) FinishUsageWith(ctx context.Context, id int64, e UsageEnd) error {
	if e.End.IsZero() {
		e.End = nowFunc()
	}
	if e.ErrClass == "" && e.ExitCode != 0 {
		e.ErrClass = ErrExitNonzero
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		var proxyID sql.NullInt64
		var cmd string
		var ended sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT proxy_id, cmd, ts_end FROM usage_log WHERE id = ?`, id).Scan(&proxyID, &cmd, &ended)
		if err == sql.ErrNoRows {
			return fmt.Errorf("usage #%d: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE usage_log SET ts_end = ?1, exit_code = ?2,
			duration_ms = max(0, ?1 - ts_start), err_class = ?3, ms = coalesce(?4, ms) WHERE id = ?5`,
			e.End.UnixMilli(), e.ExitCode, nullStr(e.ErrClass), nullInt(e.MS), id); err != nil {
			return err
		}
		if ended.Valid {
			return nil // finished twice: count it once
		}
		return bumpCountersTx(ctx, tx, proxyID.Int64, e.ExitCode, e.ErrClass, e.End, cmd)
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
	Mode      string
	ErrClass  string // "ok" = exit 0, "fail" = exit != 0, else an err_class
	Since     time.Time
	Until     time.Time
	Search    string // substring of argv, cwd or proxy URL
	Limit     int
	Offset    int
}

func (f UsageFilter) where() (string, []any) {
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
	if f.Mode != "" {
		conds = append(conds, "mode = ?")
		args = append(args, f.Mode)
	}
	switch f.ErrClass {
	case "":
	case "ok":
		conds = append(conds, "exit_code = 0")
	case "fail":
		conds = append(conds, "exit_code <> 0")
	case "proxy":
		conds = append(conds, "err_class IN ("+placeholders(len(ProxyErrClasses))+")")
		for _, c := range ProxyErrClasses {
			args = append(args, c)
		}
	default:
		conds = append(conds, "err_class = ?")
		args = append(args, f.ErrClass)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "ts_start >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "ts_start < ?")
		args = append(args, f.Until.UnixMilli())
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(s)) + "%"
		conds = append(conds, `(lower(coalesce(argv, '')) LIKE ? ESCAPE '\' OR lower(coalesce(cwd, '')) LIKE ? ESCAPE '\'
			OR lower(proxy_url) LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListUsage returns usage rows matching f, newest first.
func (d *DB) ListUsage(ctx context.Context, f UsageFilter) ([]Usage, error) {
	where, args := f.where()
	q := `SELECT id, ts_start, ts_end, proxy_id, proxy_url, mode, argv, cmd, cwd, tty, pid, ppid,
		session_id, preflight_ok, preflight_ms, exit_code, duration_ms, err_class, ms FROM usage_log` + where +
		" ORDER BY ts_start DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
		if f.Offset > 0 {
			q += " OFFSET ?"
			args = append(args, f.Offset)
		}
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
		var end, pid, ppid, proxyID, pok, pms, code, dur, msv sql.NullInt64
		var argv, cwd, tty, sess, ec sql.NullString
		if err := rows.Scan(&u.ID, &start, &end, &proxyID, &u.ProxyURL, &u.Mode, &argv, &u.Cmd, &cwd, &tty,
			&pid, &ppid, &sess, &pok, &pms, &code, &dur, &ec, &msv); err != nil {
			return nil, err
		}
		u.TSStart, u.TSEnd = time.UnixMilli(start).UTC(), fromMS(end)
		u.ProxyID, u.PID, u.PPID = proxyID.Int64, int(pid.Int64), int(ppid.Int64)
		u.Cwd, u.TTY, u.SessionID, u.ErrClass = cwd.String, tty.String, sess.String, ec.String
		u.DurationMS, u.MS = dur.Int64, int(msv.Int64)
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

// CountUsage counts the rows matching f (Limit/Offset ignored).
func (d *DB) CountUsage(ctx context.Context, f UsageFilter) (int, error) {
	where, args := f.where()
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM usage_log`+where, args...).Scan(&n)
	return n, err
}

// UsageCount is the number of usage rows for a proxy (0 = all rows).
func (d *DB) UsageCount(ctx context.Context, proxyID int64) (int, error) {
	return d.CountUsage(ctx, UsageFilter{ProxyID: proxyID})
}

// PruneUsage deletes usage rows that started before t and returns how many
// went. Proxy counters are left alone (they count every use ever); run
// RebuildStats to make them match the remaining log.
func (d *DB) PruneUsage(ctx context.Context, before time.Time) (int64, error) {
	r, err := d.sql.ExecContext(ctx, `DELETE FROM usage_log WHERE ts_start < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// ---- aggregated usage ---------------------------------------------------------

// UsageGroupKeys are the accepted UsageStats group-by keys.
var UsageGroupKeys = []string{"proxy", "cmd", "err", "mode", "session", "tty", "cwd", "day", "hour", "week", "month"}

var usageGroupExpr = map[string]string{
	"proxy":   "coalesce(nullif(u.proxy_url, ''), '-')",
	"cmd":     "coalesce(nullif(u.cmd, ''), '-')",
	"err":     "CASE WHEN u.exit_code IS NULL THEN 'running' WHEN u.exit_code = 0 THEN 'ok' ELSE coalesce(u.err_class, 'exit_nonzero') END",
	"mode":    "u.mode",
	"session": "coalesce(u.session_id, '-')",
	"tty":     "coalesce(u.tty, '-')",
	"cwd":     "coalesce(u.cwd, '-')",
	"day":     "strftime('%Y-%m-%d', u.ts_start / 1000, 'unixepoch', 'localtime')",
	"hour":    "strftime('%Y-%m-%d %H:00', u.ts_start / 1000, 'unixepoch', 'localtime')",
	"week":    "strftime('%Y-W%W', u.ts_start / 1000, 'unixepoch', 'localtime')",
	"month":   "strftime('%Y-%m', u.ts_start / 1000, 'unixepoch', 'localtime')",
}

// UsageGroup is one aggregated row of UsageStats.
type UsageGroup struct {
	Key        []string       `json:"key"`
	Uses       int            `json:"uses"`
	OK         int            `json:"ok"`
	Fail       int            `json:"fail"`
	ProxyErr   int            `json:"proxy_err"`
	Running    int            `json:"running"`
	AvgMS      float64        `json:"avg_ms,omitempty"`
	AvgDurMS   float64        `json:"avg_duration_ms,omitempty"`
	First      time.Time      `json:"first"`
	Last       time.Time      `json:"last"`
	ErrClasses map[string]int `json:"err_classes,omitempty"`
}

// Rate is ok / (ok + fail), or -1 when nothing finished.
func (g *UsageGroup) Rate() float64 {
	if g.OK+g.Fail == 0 {
		return -1
	}
	return float64(g.OK) / float64(g.OK+g.Fail)
}

// UsageStats aggregates the usage log by one or more keys (UsageGroupKeys),
// e.g. ["proxy"], ["proxy", "cmd"] or ["day"]. sort is "uses" (default),
// "last", "fail", "rate" or "key"; limit 0 = all groups.
func (d *DB) UsageStats(ctx context.Context, by []string, f UsageFilter, sort string, limit int) ([]UsageGroup, error) {
	if len(by) == 0 {
		by = []string{"proxy"}
	}
	var keys []string
	for _, k := range by {
		e, ok := usageGroupExpr[k]
		if !ok {
			return nil, fmt.Errorf("db: unknown usage group %q (want %s)", k, strings.Join(UsageGroupKeys, ", "))
		}
		keys = append(keys, e)
	}
	where, args := f.where()
	q := `SELECT ` + strings.Join(keys, ", ") + `,
		count(*),
		count(*) FILTER (WHERE u.exit_code = 0),
		count(*) FILTER (WHERE u.exit_code <> 0),
		count(*) FILTER (WHERE u.err_class IN (` + placeholders(len(ProxyErrClasses)) + `)),
		count(*) FILTER (WHERE u.exit_code IS NULL),
		avg(u.ms), avg(u.duration_ms), min(u.ts_start), max(u.ts_start)
		FROM usage_log u` + prefixCols(where) + ` GROUP BY ` + groupPositions(len(keys))
	var qargs []any
	for _, c := range ProxyErrClasses {
		qargs = append(qargs, c)
	}
	qargs = append(qargs, args...)
	switch sort {
	case "", "uses":
		q += ` ORDER BY count(*) DESC`
	case "last":
		q += ` ORDER BY max(u.ts_start) DESC`
	case "fail":
		q += ` ORDER BY count(*) FILTER (WHERE u.exit_code <> 0) DESC`
	case "rate":
		q += ` ORDER BY CAST(count(*) FILTER (WHERE u.exit_code = 0) AS REAL) / nullif(count(*) FILTER (WHERE u.exit_code IS NOT NULL), 0) DESC NULLS LAST`
	case "key":
		q += ` ORDER BY ` + groupPositions(len(keys))
	default:
		return nil, fmt.Errorf("db: unknown usage sort %q (want uses, last, fail, rate or key)", sort)
	}
	if limit > 0 {
		q += ` LIMIT ?`
		qargs = append(qargs, limit)
	}
	rows, err := d.sql.QueryContext(ctx, q, qargs...)
	if err != nil {
		return nil, fmt.Errorf("db: usage stats: %w", err)
	}
	defer rows.Close()
	var out []UsageGroup
	for rows.Next() {
		g := UsageGroup{Key: make([]string, len(keys))}
		dest := make([]any, 0, len(keys)+9)
		for i := range g.Key {
			dest = append(dest, &g.Key[i])
		}
		var avgMS, avgDur sql.NullFloat64
		var first, last int64
		dest = append(dest, &g.Uses, &g.OK, &g.Fail, &g.ProxyErr, &g.Running, &avgMS, &avgDur, &first, &last)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		g.AvgMS, g.AvgDurMS = avgMS.Float64, avgDur.Float64
		g.First, g.Last = time.UnixMilli(first).UTC(), time.UnixMilli(last).UTC()
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Error class breakdown per group (small second query, same filter).
	if len(out) > 0 {
		bq := `SELECT ` + strings.Join(keys, ", ") + `, coalesce(u.err_class, 'exit_nonzero'), count(*)
			FROM usage_log u` + prefixCols(where)
		if where == "" {
			bq += ` WHERE u.exit_code <> 0`
		} else {
			bq += ` AND u.exit_code <> 0`
		}
		bq += ` GROUP BY ` + groupPositions(len(keys)+1)
		brows, err := d.sql.QueryContext(ctx, bq, args...)
		if err != nil {
			return nil, err
		}
		defer brows.Close()
		index := map[string]int{}
		for i := range out {
			index[strings.Join(out[i].Key, "\x00")] = i
		}
		for brows.Next() {
			key := make([]string, len(keys))
			dest := make([]any, 0, len(keys)+2)
			for i := range key {
				dest = append(dest, &key[i])
			}
			var class string
			var n int
			dest = append(dest, &class, &n)
			if err := brows.Scan(dest...); err != nil {
				return nil, err
			}
			if i, ok := index[strings.Join(key, "\x00")]; ok {
				if out[i].ErrClasses == nil {
					out[i].ErrClasses = map[string]int{}
				}
				out[i].ErrClasses[class] = n
			}
		}
		if err := brows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func groupPositions(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprint(i + 1)
	}
	return strings.Join(parts, ", ")
}

// prefixCols qualifies the bare usage_log column names of a UsageFilter
// WHERE clause with the "u." alias used by the aggregate queries.
func prefixCols(where string) string {
	if where == "" {
		return ""
	}
	r := strings.NewReplacer(
		"proxy_id =", "u.proxy_id =", "cmd =", "u.cmd =", "session_id =", "u.session_id =",
		"mode =", "u.mode =", "exit_code", "u.exit_code", "err_class", "u.err_class",
		"ts_start", "u.ts_start", "coalesce(argv", "coalesce(u.argv", "coalesce(cwd", "coalesce(u.cwd",
		"lower(proxy_url)", "lower(u.proxy_url)",
	)
	return r.Replace(where)
}

// rebuildUsageCountersSQL recomputes every proxy's usage counters from
// usage_log (finished rows only).
var rebuildUsageCountersSQL = `UPDATE proxies SET
	use_count     = coalesce((SELECT count(*) FROM usage_log u WHERE u.proxy_id = proxies.id AND u.exit_code IS NOT NULL), 0),
	use_ok        = coalesce((SELECT count(*) FROM usage_log u WHERE u.proxy_id = proxies.id AND u.exit_code = 0), 0),
	use_fail      = coalesce((SELECT count(*) FROM usage_log u WHERE u.proxy_id = proxies.id AND u.exit_code <> 0), 0),
	use_proxy_err = coalesce((SELECT count(*) FROM usage_log u WHERE u.proxy_id = proxies.id AND u.err_class IN ('` +
	strings.Join(ProxyErrClasses, "','") + `')), 0),
	last_used_at  = (SELECT max(coalesce(u.ts_end, u.ts_start)) FROM usage_log u WHERE u.proxy_id = proxies.id AND u.exit_code IS NOT NULL),
	last_cmd      = (SELECT u.cmd FROM usage_log u WHERE u.proxy_id = proxies.id AND u.exit_code IS NOT NULL
	                 ORDER BY coalesce(u.ts_end, u.ts_start) DESC, u.id DESC LIMIT 1)`
