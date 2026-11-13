package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// listedExpr counts the distinct lists that list a proxy's host or exit IP.
const listedExpr = `(SELECT count(DISTINCT r.list_name) FROM reputation r
	WHERE r.listed = 1 AND (r.ip = proxies.host OR r.ip = proxies.exit_ip))`

// updateListedAllSQL recomputes listed_count for every proxy.
const updateListedAllSQL = `UPDATE proxies SET listed_count = ` + listedExpr

// updateListedIPSQL recomputes listed_count for proxies whose host or exit
// IP is the given ip.
const updateListedIPSQL = `UPDATE proxies SET listed_count = ` + listedExpr + `
	WHERE host = ?1 OR exit_ip = ?1`

const upsertReputationSQL = `INSERT INTO reputation (ip, list_name, list_kind, listed, detail, checked_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (ip, list_name) DO UPDATE SET
	list_kind = excluded.list_kind, listed = excluded.listed,
	detail = excluded.detail, checked_at = excluded.checked_at`

// reputationTx upserts verdicts and refreshes listed_count of the proxies
// they concern.
func reputationTx(ctx context.Context, tx *sql.Tx, rs []Reputation) error {
	up, err := tx.PrepareContext(ctx, upsertReputationSQL)
	if err != nil {
		return err
	}
	defer up.Close()
	listed, err := tx.PrepareContext(ctx, updateListedIPSQL)
	if err != nil {
		return err
	}
	defer listed.Close()
	now := nowFunc()
	ips := map[string]bool{}
	var order []string
	for i := range rs {
		r := &rs[i]
		if r.IP == "" || r.ListName == "" {
			return fmt.Errorf("db: reputation needs ip and list_name")
		}
		if !slices.Contains(ListKinds, r.ListKind) {
			return fmt.Errorf("db: reputation list_kind %q (want one of %v)", r.ListKind, ListKinds)
		}
		if r.CheckedAt.IsZero() {
			r.CheckedAt = now
		}
		if _, err := up.ExecContext(ctx, r.IP, r.ListName, r.ListKind, r.Listed, nullStr(r.Detail),
			r.CheckedAt.UnixMilli()); err != nil {
			return fmt.Errorf("db: reputation %s/%s: %w", r.IP, r.ListName, err)
		}
		if !ips[r.IP] {
			ips[r.IP] = true
			order = append(order, r.IP)
		}
	}
	for _, ip := range order {
		if _, err := listed.ExecContext(ctx, ip); err != nil {
			return err
		}
	}
	return nil
}

// UpsertReputation stores verdicts (one per ip+list) and updates the
// listed_count of matching proxies in the same transaction.
func (d *DB) UpsertReputation(ctx context.Context, rs ...Reputation) error {
	if len(rs) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *sql.Tx) error { return reputationTx(ctx, tx, rs) })
}

// ReputationFor returns every verdict about ip, listed ones first.
func (d *DB) ReputationFor(ctx context.Context, ips ...string) ([]Reputation, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	args := make([]any, len(ips))
	for i, ip := range ips {
		args[i] = ip
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT id, ip, list_name, list_kind, listed, detail, checked_at
		FROM reputation WHERE ip IN (`+placeholders(len(ips))+`) ORDER BY listed DESC, list_name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reputation
	for rows.Next() {
		var r Reputation
		var detail sql.NullString
		var at int64
		if err := rows.Scan(&r.ID, &r.IP, &r.ListName, &r.ListKind, &r.Listed, &detail, &at); err != nil {
			return nil, err
		}
		r.Detail, r.CheckedAt = detail.String, time.UnixMilli(at).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
