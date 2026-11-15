package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// summary is the denormalised state on a proxies row that measurements
// drive. fold is the single definition of how one fact changes it, shared
// by the incremental path (RecordMeasurement, Writer) and RebuildStats, so
// both always agree.
type summary struct {
	status     string
	checkedAt  int64 // unix ms, 0 = never
	lastMS     int
	bestMS     int
	msCount    int
	msSum      int64
	okCount    int
	failCount  int
	okStreak   int
	failStreak int
	exitIP     string
}

// fold applies one measurement:
//   - status follows the latest inserted fact (alive on success, dead on
//     failure); facts are folded in insertion order, not timestamp order,
//   - checked_at is the newest fact timestamp,
//   - last/best/avg ms only consider timed successes (avg = ms_sum/ms_count),
//   - ok/fail counts and streaks (a success resets the fail streak and vice
//     versa),
//   - exit_ip is the latest non-empty one reported.
func (s *summary) fold(m *Measurement) {
	ts := m.TS.UnixMilli()
	if ts > s.checkedAt {
		s.checkedAt = ts
	}
	if m.ExitIP != "" {
		s.exitIP = m.ExitIP
	}
	if IsAuxProbe(m.Probe) {
		return // kept as a fact, but liveness / latency come from connectivity checks
	}
	if m.OK {
		s.status = StatusAlive
		s.okCount++
		s.okStreak++
		s.failStreak = 0
		if m.MS > 0 {
			s.lastMS = m.MS
			if s.bestMS == 0 || m.MS < s.bestMS {
				s.bestMS = m.MS
			}
			s.msCount++
			s.msSum += int64(m.MS)
		}
	} else {
		s.status = StatusDead
		s.failCount++
		s.failStreak++
		s.okStreak = 0
	}
}

// Auxiliary probe names: scan checks that test something beyond "does the
// proxy carry traffic" (TLS to one site, an anonymity judge, the exit-IP
// lookup). Their facts are stored like any other, but fold ignores them
// for status, counts, streaks and latency (exit_ip and checked_at still
// update): a proxy that answered every connectivity probe must not turn
// "dead" because a judge site refused it at the end of a full scan.
var AuxProbes = []string{"https", "judge", "exit-ip"}

// IsAuxProbe reports whether name is one of AuxProbes.
func IsAuxProbe(name string) bool { return slices.Contains(AuxProbes, name) }

// auxProbeSQL is a SQL list literal of AuxProbes ("'https','judge',...").
func auxProbeSQL() string {
	q := make([]string, len(AuxProbes))
	for i, p := range AuxProbes {
		q[i] = "'" + p + "'"
	}
	return strings.Join(q, ",")
}

// NotAuxCond is a WHERE fragment excluding auxiliary facts (column probe).
func NotAuxCond() string { return "probe NOT IN (" + auxProbeSQL() + ")" }

func (s *summary) avg() any {
	if s.msCount == 0 {
		return nil
	}
	return float64(s.msSum) / float64(s.msCount)
}

const loadSummarySQL = `SELECT status, coalesce(checked_at, 0), coalesce(last_ms, 0), coalesce(best_ms, 0),
	ms_count, ms_sum, ok_count, fail_count, ok_streak, fail_streak, coalesce(exit_ip, '')
	FROM proxies WHERE id = ?`

const saveSummarySQL = `UPDATE proxies SET status = ?, checked_at = ?, last_ms = ?, best_ms = ?, avg_ms = ?,
	ms_count = ?, ms_sum = ?, ok_count = ?, fail_count = ?, ok_streak = ?, fail_streak = ?, exit_ip = ?
	WHERE id = ?`

func saveSummary(ctx context.Context, stmt *sql.Stmt, id int64, s *summary) error {
	var checked any
	if s.checkedAt > 0 {
		checked = s.checkedAt
	}
	status := s.status
	if status == "" {
		status = StatusUnknown
	}
	_, err := stmt.ExecContext(ctx, status, checked, nullInt(s.lastMS), nullInt(s.bestMS), s.avg(),
		s.msCount, s.msSum, s.okCount, s.failCount, s.okStreak, s.failStreak, nullStr(s.exitIP), id)
	return err
}

func validKind(k string) bool { return slices.Contains(Kinds, k) }

// recordTx inserts the facts and updates the summaries of the affected
// proxies inside tx. Measurements with ProxyID 0 resolve (or create) their
// proxy from Addr. Summaries are folded in slice order, one UPDATE per
// proxy.
func recordTx(ctx context.Context, tx *sql.Tx, ms []Measurement) error {
	ins, err := tx.PrepareContext(ctx, `INSERT INTO measurements
		(proxy_id, ts, kind, probe, ok, ms, err_class, exit_ip, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	var upsert *sql.Stmt
	defer func() {
		if upsert != nil {
			upsert.Close()
		}
	}()

	now := nowFunc()
	order := make([]int64, 0, len(ms))
	byProxy := make(map[int64][]*Measurement, len(ms))
	for i := range ms {
		m := &ms[i]
		if m.Kind == "" {
			m.Kind = KindProbe
		}
		if !validKind(m.Kind) {
			return fmt.Errorf("db: measurement kind %q (want one of %v)", m.Kind, Kinds)
		}
		if m.TS.IsZero() {
			m.TS = now
		}
		if m.ProxyID == 0 {
			if upsert == nil {
				if upsert, err = tx.PrepareContext(ctx, upsertProxySQL); err != nil {
					return err
				}
			}
			if m.ProxyID, err = upsertProxyTx(ctx, upsert, m.Addr, "", now, time.Time{}); err != nil {
				return err
			}
		}
		r, err := ins.ExecContext(ctx, m.ProxyID, m.TS.UnixMilli(), m.Kind, m.Probe, m.OK,
			nullInt(m.MS), nullStr(m.ErrClass), nullStr(m.ExitIP), nullStr(m.Detail))
		if err != nil {
			return fmt.Errorf("db: insert measurement for proxy #%d: %w", m.ProxyID, err)
		}
		m.ID, _ = r.LastInsertId()
		if _, ok := byProxy[m.ProxyID]; !ok {
			order = append(order, m.ProxyID)
		}
		byProxy[m.ProxyID] = append(byProxy[m.ProxyID], m)
	}

	load, err := tx.PrepareContext(ctx, loadSummarySQL)
	if err != nil {
		return err
	}
	defer load.Close()
	save, err := tx.PrepareContext(ctx, saveSummarySQL)
	if err != nil {
		return err
	}
	defer save.Close()
	for _, id := range order {
		var s summary
		if err := load.QueryRowContext(ctx, id).Scan(&s.status, &s.checkedAt, &s.lastMS, &s.bestMS,
			&s.msCount, &s.msSum, &s.okCount, &s.failCount, &s.okStreak, &s.failStreak, &s.exitIP); err != nil {
			return fmt.Errorf("db: load summary of proxy #%d: %w", id, err)
		}
		for _, m := range byProxy[id] {
			s.fold(m)
		}
		if err := saveSummary(ctx, save, id, &s); err != nil {
			return fmt.Errorf("db: update summary of proxy #%d: %w", id, err)
		}
	}
	return nil
}

// RecordMeasurement inserts one fact and updates the proxy's summary
// columns in the same transaction. Bulk producers use a Writer instead.
func (d *DB) RecordMeasurement(ctx context.Context, m Measurement) (int64, error) {
	ms := []Measurement{m}
	err := d.Tx(ctx, func(tx *sql.Tx) error { return recordTx(ctx, tx, ms) })
	return ms[0].ID, err
}

// RecordMeasurements is RecordMeasurement for several facts in one
// transaction.
func (d *DB) RecordMeasurements(ctx context.Context, ms []Measurement) error {
	if len(ms) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *sql.Tx) error { return recordTx(ctx, tx, ms) })
}

// Measurements returns a proxy's most recent facts, newest first
// (limit <= 0 = all).
func (d *DB) Measurements(ctx context.Context, proxyID int64, limit int) ([]Measurement, error) {
	q := `SELECT id, proxy_id, ts, kind, probe, ok, ms, err_class, exit_ip, detail
		FROM measurements WHERE proxy_id = ? ORDER BY id DESC`
	args := []any{proxyID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Measurement
	for rows.Next() {
		var m Measurement
		var ts int64
		var msv sql.NullInt64
		var ec, ip, detail sql.NullString
		if err := rows.Scan(&m.ID, &m.ProxyID, &ts, &m.Kind, &m.Probe, &m.OK, &msv, &ec, &ip, &detail); err != nil {
			return nil, err
		}
		m.TS = time.UnixMilli(ts).UTC()
		m.MS, m.ErrClass, m.ExitIP, m.Detail = int(msv.Int64), ec.String, ip.String, detail.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// RebuildResult reports RebuildStats.
type RebuildResult struct {
	Proxies      int           `json:"proxies"`
	Measurements int           `json:"measurements"`
	Elapsed      time.Duration `json:"elapsed_ns"`
}

// RebuildStats recomputes every proxy's summary columns (and listed_count)
// from the fact tables, in one transaction. Facts are folded in insertion
// order, exactly as they were applied incrementally, so the result equals
// the incremental state.
func (d *DB) RebuildStats(ctx context.Context) (RebuildResult, error) {
	start := time.Now()
	var res RebuildResult
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE proxies SET status = 'unknown', checked_at = NULL,
			last_ms = NULL, best_ms = NULL, avg_ms = NULL, ms_count = 0, ms_sum = 0, ok_count = 0,
			fail_count = 0, ok_streak = 0, fail_streak = 0, exit_ip = NULL`); err != nil {
			return err
		}
		save, err := tx.PrepareContext(ctx, saveSummarySQL)
		if err != nil {
			return err
		}
		defer save.Close()
		rows, err := tx.QueryContext(ctx, `SELECT proxy_id, ts, ok, coalesce(ms, 0), coalesce(exit_ip, ''), probe
			FROM measurements ORDER BY proxy_id, id`)
		if err != nil {
			return err
		}
		var cur int64
		var s summary
		flush := func() error {
			if cur == 0 {
				return nil
			}
			res.Proxies++
			return saveSummary(ctx, save, cur, &s)
		}
		// Collect first: the driver cannot interleave an open cursor with
		// writes on the same transaction reliably.
		type fact struct {
			id int64
			m  Measurement
		}
		var facts []fact
		for rows.Next() {
			var f fact
			var ts int64
			if err := rows.Scan(&f.id, &ts, &f.m.OK, &f.m.MS, &f.m.ExitIP, &f.m.Probe); err != nil {
				rows.Close()
				return err
			}
			f.m.TS = time.UnixMilli(ts)
			facts = append(facts, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range facts {
			if facts[i].id != cur {
				if err := flush(); err != nil {
					return err
				}
				cur, s = facts[i].id, summary{}
			}
			s.fold(&facts[i].m)
			res.Measurements++
		}
		if err := flush(); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, updateListedAllSQL); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, rebuildUsageCountersSQL)
		return err
	})
	res.Elapsed = time.Since(start)
	return res, err
}
