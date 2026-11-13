package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SetTag adds or refreshes a tag on a proxy. since keeps its original value
// when the tag already exists (only reason is updated).
func (d *DB) SetTag(ctx context.Context, t Tag) error {
	return d.SetTags(ctx, []Tag{t})
}

// SetTags is SetTag for many tags in one transaction.
func (d *DB) SetTags(ctx context.Context, ts []Tag) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO tags (proxy_id, tag, scope, since, reason)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (proxy_id, tag, scope) DO UPDATE SET reason = excluded.reason`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		now := nowFunc()
		for _, t := range ts {
			if t.ProxyID == 0 || t.Tag == "" {
				return fmt.Errorf("db: tag needs proxy_id and tag")
			}
			since := t.Since
			if since.IsZero() {
				since = now
			}
			if _, err := stmt.ExecContext(ctx, t.ProxyID, t.Tag, t.Scope, since.UnixMilli(), t.Reason); err != nil {
				return fmt.Errorf("db: tag %s on #%d: %w", t.Label(), t.ProxyID, err)
			}
		}
		return nil
	})
}

// ClearTag removes one tag (scope "" = the unscoped tag) from a proxy.
func (d *DB) ClearTag(ctx context.Context, proxyID int64, tag, scope string) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM tags WHERE proxy_id = ? AND tag = ? AND scope = ?`, proxyID, tag, scope)
	return err
}

// ClearTags removes tag from every proxy (all scopes when scope is "*"),
// returning how many rows were deleted. Heuristic re-tagging clears and
// re-sets a tag in one pass.
func (d *DB) ClearTags(ctx context.Context, tag, scope string) (int64, error) {
	q, args := `DELETE FROM tags WHERE tag = ?`, []any{tag}
	if scope != "*" {
		q += ` AND scope = ?`
		args = append(args, scope)
	}
	r, err := d.sql.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// ReplaceTags atomically recomputes heuristic tags: every row whose tag is
// in tagNames (any scope) is deleted and ts is inserted, in one
// transaction. A (proxy, tag, scope) that survives keeps its original since.
func (d *DB) ReplaceTags(ctx context.Context, tagNames []string, ts []Tag) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		old := map[[3]string]int64{}
		for _, name := range tagNames {
			rows, err := tx.QueryContext(ctx, `SELECT proxy_id, scope, since FROM tags WHERE tag = ?`, name)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, since int64
				var scope string
				if err := rows.Scan(&id, &scope, &since); err != nil {
					rows.Close()
					return err
				}
				old[[3]string{fmt.Sprint(id), name, scope}] = since
			}
			rows.Close()
			if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE tag = ?`, name); err != nil {
				return err
			}
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO tags (proxy_id, tag, scope, since, reason) VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		now := nowFunc().UnixMilli()
		for _, t := range ts {
			since := now
			if !t.Since.IsZero() {
				since = t.Since.UnixMilli()
			}
			if s, ok := old[[3]string{fmt.Sprint(t.ProxyID), t.Tag, t.Scope}]; ok {
				since = s
			}
			if _, err := stmt.ExecContext(ctx, t.ProxyID, t.Tag, t.Scope, since, t.Reason); err != nil {
				return err
			}
		}
		return nil
	})
}

// Tags returns a proxy's tags (proxyID 0 = every tag row), ordered by tag.
func (d *DB) Tags(ctx context.Context, proxyID int64) ([]Tag, error) {
	q, args := `SELECT proxy_id, tag, scope, since, reason FROM tags`, []any{}
	if proxyID > 0 {
		q += ` WHERE proxy_id = ?`
		args = append(args, proxyID)
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY tag, scope, proxy_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		var since int64
		if err := rows.Scan(&t.ProxyID, &t.Tag, &t.Scope, &since, &t.Reason); err != nil {
			return nil, err
		}
		t.Since = time.UnixMilli(since).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagCount is one tag label with how many proxies carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Scope string `json:"scope,omitempty"`
	Count int    `json:"count"`
}

// TagCounts lists every (tag, scope) with its proxy count.
func (d *DB) TagCounts(ctx context.Context) ([]TagCount, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT tag, scope, count(*) FROM tags GROUP BY tag, scope ORDER BY tag, scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var t TagCount
		if err := rows.Scan(&t.Tag, &t.Scope, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
