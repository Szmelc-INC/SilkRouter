package score

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

// Heuristic tag names. Every other tag is manual and never touched by a
// refresh (e.g. "blocked", which route skips by default, or "pinned").
const (
	TagStable      = "stable"      // last N checks / uses all succeeded
	TagFast        = "fast"        // last N timed successes all under fast_ms
	TagFav         = "fav"         // scope = command: used often, works
	TagRecommended = "recommended" // scope "" = best ranked; scope = command: best for it
	TagClean       = "clean"       // full-scanned, on no list, not transparent
	TagElite       = "elite"       // anonymity elite
	TagHTTPS       = "https"       // TLS through the proxy works
	TagFlaky       = "flaky"       // alive but fails often
	TagTor         = "tor"         // a Tor exit
	TagDatacenter  = "datacenter"  // hosting / datacenter range
	TagKnown       = "known"       // on a known-proxy list (sites block these)
)

// HeuristicTags lists every tag a refresh owns.
var HeuristicTags = []string{TagStable, TagFast, TagFav, TagRecommended, TagClean, TagElite,
	TagHTTPS, TagFlaky, TagTor, TagDatacenter, TagKnown}

// IsHeuristic reports whether tag is computed (not manual).
func IsHeuristic(tag string) bool {
	for _, t := range HeuristicTags {
		if t == tag {
			return true
		}
	}
	return false
}

// TagOptions are the thresholds (config [tags]).
type TagOptions struct {
	StableN       int
	FastN         int
	FastMS        int
	FavMinUses    int
	FavMinRate    float64
	RecommendTop  int
	FlakyMinFacts int
	FlakyMaxRate  float64
	Window        time.Duration // usage window (0 = all)
}

func (o *TagOptions) defaults() {
	if o.StableN <= 0 {
		o.StableN = 10
	}
	if o.FastN <= 0 {
		o.FastN = 10
	}
	if o.FastMS <= 0 {
		o.FastMS = 100
	}
	if o.FavMinUses <= 0 {
		o.FavMinUses = 10
	}
	if o.FavMinRate <= 0 {
		o.FavMinRate = 0.8
	}
	if o.FlakyMinFacts <= 0 {
		o.FlakyMinFacts = 10
	}
	if o.FlakyMaxRate <= 0 {
		o.FlakyMaxRate = 0.5
	}
}

// TagResult counts the tags a refresh set.
type TagResult struct {
	Counts  map[string]int `json:"counts"`
	Total   int            `json:"total"`
	Elapsed time.Duration  `json:"elapsed_ns"`
}

// RefreshTags recomputes every heuristic tag of every proxy (one
// transaction; manual tags are left alone).
func RefreshTags(ctx context.Context, d *db.DB, o TagOptions) (TagResult, error) {
	start := time.Now()
	o.defaults()
	ts, err := computeTags(ctx, d.SQL(), o, nil, "")
	if err != nil {
		return TagResult{}, err
	}
	if err := d.ReplaceTags(ctx, HeuristicTags, ts); err != nil {
		return TagResult{}, err
	}
	res := TagResult{Counts: map[string]int{}, Total: len(ts), Elapsed: time.Since(start)}
	for _, t := range ts {
		res.Counts[t.Tag]++
	}
	return res, nil
}

// RefreshFor recomputes the per-proxy tags of one proxy (stable, fast,
// fav:*, flaky, ...) and recommended:<cmd> for cmd, after that proxy was
// used. It is cheap enough to run after every route.
func RefreshFor(ctx context.Context, d *db.DB, o TagOptions, proxyID int64, cmd string) error {
	o.defaults()
	ts, err := computeTags(ctx, d.SQL(), o, []int64{proxyID}, "")
	if err != nil {
		return err
	}
	perProxy := []string{TagStable, TagFast, TagFav, TagClean, TagElite, TagHTTPS, TagFlaky, TagTor, TagDatacenter, TagKnown}
	var mine []db.Tag
	for _, t := range ts {
		if t.Tag != TagRecommended {
			mine = append(mine, t)
		}
	}
	if err := d.ReplaceTagSet(ctx, db.TagSelection{Tags: perProxy, ProxyIDs: []int64{proxyID}}, mine); err != nil {
		return err
	}
	if cmd == "" || o.RecommendTop <= 0 {
		return nil
	}
	rec, err := recommendFor(ctx, d.SQL(), o, cmd)
	if err != nil {
		return err
	}
	return d.ReplaceTagSet(ctx, db.TagSelection{Tags: []string{TagRecommended}, Scope: &cmd}, rec)
}

// computeTags derives the heuristic tags (ids nil = every proxy).
func computeTags(ctx context.Context, q *sql.DB, o TagOptions, ids []int64, _ string) ([]db.Tag, error) {
	var out []db.Tag
	idCond, idArgs := "", []any{}
	if len(ids) > 0 {
		idCond = " AND proxy_id IN (" + marks(len(ids)) + ")"
		for _, id := range ids {
			idArgs = append(idArgs, id)
		}
	}
	pCond := ""
	if len(ids) > 0 {
		pCond = " AND p.id IN (" + marks(len(ids)) + ")"
	}

	// stable: the last N facts all succeeded.
	err := collect(ctx, q, &out, func(id int64, extra []any) db.Tag {
		return db.Tag{ProxyID: id, Tag: TagStable, Reason: fmt.Sprintf("last %d checks / uses ok", o.StableN)}
	}, `SELECT proxy_id FROM (
			SELECT proxy_id, ok, row_number() OVER (PARTITION BY proxy_id ORDER BY id DESC) AS rn
			FROM measurements WHERE 1=1`+idCond+`)
		WHERE rn <= ? GROUP BY proxy_id HAVING count(*) = ? AND sum(ok) = ?`,
		append(append([]any{}, idArgs...), o.StableN, o.StableN, o.StableN)...)
	if err != nil {
		return nil, fmt.Errorf("tags: stable: %w", err)
	}

	// fast: the last N timed successes were all under fast_ms.
	err = collect(ctx, q, &out, func(id int64, extra []any) db.Tag {
		return db.Tag{ProxyID: id, Tag: TagFast, Reason: fmt.Sprintf("last %d timed successes < %dms (max %vms)", o.FastN, o.FastMS, extra[0])}
	}, `SELECT proxy_id, max(ms) FROM (
			SELECT proxy_id, ms, row_number() OVER (PARTITION BY proxy_id ORDER BY id DESC) AS rn
			FROM measurements WHERE ok = 1 AND ms > 0`+idCond+`)
		WHERE rn <= ? GROUP BY proxy_id HAVING count(*) = ? AND max(ms) < ?`,
		append(append([]any{}, idArgs...), o.FastN, o.FastN, o.FastMS)...)
	if err != nil {
		return nil, fmt.Errorf("tags: fast: %w", err)
	}

	// fav:<cmd>: used at least N times for cmd, success rate above the bar.
	since := int64(0)
	if o.Window > 0 {
		since = time.Now().Add(-o.Window).UnixMilli()
	}
	rows, err := q.QueryContext(ctx, `SELECT proxy_id, cmd, count(*), sum(exit_code = 0) FROM usage_log
		WHERE proxy_id IS NOT NULL AND exit_code IS NOT NULL AND cmd <> '' AND ts_start >= ?`+idCond+`
		GROUP BY proxy_id, cmd HAVING count(*) >= ?`, append(append([]any{since}, idArgs...), o.FavMinUses)...)
	if err != nil {
		return nil, fmt.Errorf("tags: fav: %w", err)
	}
	for rows.Next() {
		var id int64
		var cmd string
		var n, ok int
		if err := rows.Scan(&id, &cmd, &n, &ok); err != nil {
			rows.Close()
			return nil, err
		}
		if rate := float64(ok) / float64(n); rate >= o.FavMinRate {
			out = append(out, db.Tag{ProxyID: id, Tag: TagFav, Scope: cmd,
				Reason: fmt.Sprintf("%d uses for %s, %.0f%% ok", n, cmd, 100*rate)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attribute tags straight from the proxies row.
	rows, err = q.QueryContext(ctx, `SELECT p.id, p.status, p.anonymity, p.https_ok, p.hosting, p.full_scan_at,
		p.listed_count, p.ok_count, p.fail_count,
		(SELECT count(DISTINCT r.list_name) FROM reputation r WHERE r.listed = 1 AND r.list_kind = 'torexit' AND (r.ip = p.host OR r.ip = p.exit_ip)),
		(SELECT count(DISTINCT r.list_name) FROM reputation r WHERE r.listed = 1 AND r.list_kind = 'datacenter' AND (r.ip = p.host OR r.ip = p.exit_ip)),
		(SELECT count(DISTINCT r.list_name) FROM reputation r WHERE r.listed = 1 AND r.list_kind = 'proxylist' AND (r.ip = p.host OR r.ip = p.exit_ip))
		FROM proxies p WHERE 1=1`+pCond, idArgs...)
	if err != nil {
		return nil, fmt.Errorf("tags: attributes: %w", err)
	}
	for rows.Next() {
		var id int64
		var status, anon string
		var httpsOK, hosting, fullScan sql.NullInt64
		var listed, okN, failN, tor, dc, known int
		if err := rows.Scan(&id, &status, &anon, &httpsOK, &hosting, &fullScan, &listed, &okN, &failN, &tor, &dc, &known); err != nil {
			rows.Close()
			return nil, err
		}
		if fullScan.Valid && listed == 0 && (anon == "elite" || anon == "anonymous") {
			out = append(out, db.Tag{ProxyID: id, Tag: TagClean, Reason: "full scan: on no list, " + anon})
		}
		if anon == "elite" {
			out = append(out, db.Tag{ProxyID: id, Tag: TagElite, Reason: "no proxy headers, real IP hidden"})
		}
		if httpsOK.Valid && httpsOK.Int64 != 0 {
			out = append(out, db.Tag{ProxyID: id, Tag: TagHTTPS, Reason: "TLS through the proxy works"})
		}
		if n := okN + failN; status == db.StatusAlive && n >= o.FlakyMinFacts && float64(okN)/float64(n) < o.FlakyMaxRate {
			out = append(out, db.Tag{ProxyID: id, Tag: TagFlaky, Reason: fmt.Sprintf("%d/%d checks ok", okN, n)})
		}
		if tor > 0 {
			out = append(out, db.Tag{ProxyID: id, Tag: TagTor, Reason: "listed as a Tor exit"})
		}
		if dc > 0 || (hosting.Valid && hosting.Int64 != 0) {
			out = append(out, db.Tag{ProxyID: id, Tag: TagDatacenter, Reason: "hosting / datacenter range"})
		}
		if known > 0 {
			out = append(out, db.Tag{ProxyID: id, Tag: TagKnown, Reason: fmt.Sprintf("on %d known-proxy list(s)", known)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(ids) > 0 || o.RecommendTop <= 0 {
		return out, nil
	}
	// recommended: the best ranked alive proxies.
	err = collect(ctx, q, &out, func(id int64, extra []any) db.Tag {
		return db.Tag{ProxyID: id, Tag: TagRecommended, Reason: fmt.Sprintf("rank %v", extra[0])}
	}, `SELECT id, rank FROM proxies WHERE status = 'alive' AND rank IS NOT NULL ORDER BY rank LIMIT ?`, o.RecommendTop)
	if err != nil {
		return nil, fmt.Errorf("tags: recommended: %w", err)
	}
	// recommended:<cmd> for every command with usage.
	cmds, err := commands(ctx, q, since)
	if err != nil {
		return nil, err
	}
	for _, cmd := range cmds {
		rec, err := recommendFor(ctx, q, o, cmd)
		if err != nil {
			return nil, err
		}
		out = append(out, rec...)
	}
	return out, nil
}

// recommendFor picks the best proxies for cmd among the alive ones used for
// it: smoothed usage success rate, then speed / balanced score.
func recommendFor(ctx context.Context, q *sql.DB, o TagOptions, cmd string) ([]db.Tag, error) {
	since := int64(0)
	if o.Window > 0 {
		since = time.Now().Add(-o.Window).UnixMilli()
	}
	rows, err := q.QueryContext(ctx, `SELECT u.proxy_id, count(*), sum(u.exit_code = 0), avg(u.ms),
		coalesce(p.speed_score, 0), coalesce(p.clean_score, 50)
		FROM usage_log u JOIN proxies p ON p.id = u.proxy_id
		WHERE u.cmd = ? AND u.exit_code IS NOT NULL AND u.ts_start >= ? AND p.status = 'alive'
		AND NOT EXISTS (SELECT 1 FROM tags t WHERE t.proxy_id = p.id AND t.tag = 'blocked')
		GROUP BY u.proxy_id`, cmd, since)
	if err != nil {
		return nil, fmt.Errorf("tags: recommended:%s: %w", cmd, err)
	}
	defer rows.Close()
	type cand struct {
		id    int64
		n, ok int
		ms    sql.NullFloat64
		score float64
	}
	var cs []cand
	for rows.Next() {
		var c cand
		var speed, cleanScore float64
		if err := rows.Scan(&c.id, &c.n, &c.ok, &c.ms, &speed, &cleanScore); err != nil {
			return nil, err
		}
		rate := (float64(c.ok) + 1) / (float64(c.n) + 2)
		confidence := float64(min(c.n, 20)) / 20
		c.score = 0.6*rate*(0.5+0.5*confidence) + 0.3*speed/100 + 0.1*cleanScore/100
		cs = append(cs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].score != cs[j].score {
			return cs[i].score > cs[j].score
		}
		return cs[i].id < cs[j].id
	})
	var out []db.Tag
	for i, c := range cs {
		if i >= o.RecommendTop {
			break
		}
		if c.ok == 0 {
			continue
		}
		reason := fmt.Sprintf("#%d for %s: %d/%d ok", i+1, cmd, c.ok, c.n)
		if c.ms.Valid && c.ms.Float64 > 0 {
			reason += fmt.Sprintf(", ~%.0fms", c.ms.Float64)
		}
		out = append(out, db.Tag{ProxyID: c.id, Tag: TagRecommended, Scope: cmd, Reason: reason})
	}
	return out, nil
}

func commands(ctx context.Context, q *sql.DB, since int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT cmd FROM usage_log WHERE cmd <> '' AND proxy_id IS NOT NULL AND ts_start >= ? ORDER BY cmd`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// collect runs query (first column = proxy id, the rest passed to mk) and
// appends a tag per row.
func collect(ctx context.Context, q *sql.DB, out *[]db.Tag, mk func(id int64, extra []any) db.Tag, query string, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		extra := make([]any, len(cols)-1)
		dest := []any{&id}
		for i := range extra {
			dest = append(dest, &extra[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		*out = append(*out, mk(id, extra))
	}
	return rows.Err()
}

func marks(n int) string {
	s := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			s = append(s, ',')
		}
		s = append(s, '?')
	}
	return string(s)
}
