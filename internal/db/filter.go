package db

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Filter selects proxies for ListProxies / CountProxies / Export. The zero
// value matches every proxy, sorted by id.
type Filter struct {
	IDs     []int64
	Status  []string // StatusAlive, ... (empty = any)
	Schemes []string // exact schemes; see ExpandSchemes for "socks"/"http" groups
	// MinMS / MaxMS bound last_ms (MaxMS 0 = no upper bound). Proxies whose
	// last_ms is unknown pass only when IncludeUntimed is set and MinMS is 0
	// (the legacy text-cache rule).
	MinMS, MaxMS   int
	IncludeUntimed bool
	Tag            string // proxies carrying this tag ...
	TagScope       string // ... in this scope ("" = any scope)
	Countries      []string
	ListedMax      *int // at most this many reputation listings
	MinOKStreak    int
	Source         string // exact source label
	// Search matches a substring of host, scheme://host:port, exit IP,
	// country, org or source (case-insensitive).
	Search       string
	CheckedSince time.Time // checked at or after
	// ExcludeTags drops proxies carrying any of these tags (any scope).
	ExcludeTags []string
	// Anonymity keeps only these anonymity grades.
	Anonymity []string
	// HTTPSOnly keeps proxies whose TLS check succeeded.
	HTTPSOnly bool
	// AfterID keeps ids greater than this (resumable scans walk by id).
	AfterID int64
	// FullScanBefore keeps proxies never full-scanned or full-scanned before
	// this time.
	FullScanBefore time.Time
	// Sort is one of SortKeys (default "id"); each key has a natural
	// direction (latency ascending, scores descending, ...). Reverse flips
	// it. Unknown values (NULL) always sort last. "random" shuffles.
	Sort    string
	Reverse bool
	Limit   int // 0 = all
	Offset  int
	// WithTags fills Proxy.Tags.
	WithTags bool
}

// sortSpec is a sort key's column expression and natural direction.
type sortSpec struct {
	expr string
	desc bool
	help string
}

var sortKeys = map[string]sortSpec{
	"id":           {"p.id", false, "insertion order"},
	"rank":         {"p.rank", false, "scan rank (1 = best)"},
	"last_ms":      {"p.last_ms", false, "latest measured latency"},
	"best_ms":      {"p.best_ms", false, "best latency ever"},
	"avg_ms":       {"p.avg_ms", false, "average latency"},
	"speed_score":  {"p.speed_score", true, "speed score (higher = faster)"},
	"clean_score":  {"p.clean_score", true, "reputation score (higher = cleaner)"},
	"ok_streak":    {"p.ok_streak", true, "consecutive successes"},
	"fail_streak":  {"p.fail_streak", true, "consecutive failures"},
	"ok_count":     {"p.ok_count", true, "total successes"},
	"fail_count":   {"p.fail_count", true, "total failures"},
	"success_rate": {"(CAST(p.ok_count AS REAL) / nullif(p.ok_count + p.fail_count, 0))", true, "ok / (ok + fail)"},
	"listed":       {"p.listed_count", false, "reputation listings"},
	"checked_at":   {"p.checked_at", true, "most recently checked first"},
	"added_at":     {"p.added_at", true, "newest first"},
	"host":         {"p.host", false, "host name / IP"},
	"scheme":       {"p.scheme", false, "protocol"},
	"country":      {"p.country", false, "exit country"},
	"random":       {"random()", false, "random order"},
	"stable_score": {"p.stable_score", true, "stability score (higher = steadier)"},
	"jitter_ms":    {"p.jitter_ms", false, "latency jitter of the last full scan"},
	"use_count":    {"p.use_count", true, "times used"},
	"use_rate":     {"(CAST(p.use_ok AS REAL) / nullif(p.use_count, 0))", true, "share of uses that exited 0"},
	"last_used_at": {"p.last_used_at", true, "most recently used first"},
	"full_scan_at": {"p.full_scan_at", true, "most recently full-scanned first"},
	"anonymity":    {"CASE p.anonymity WHEN 'elite' THEN 0 WHEN 'anonymous' THEN 1 WHEN 'transparent' THEN 2 END", false, "elite, anonymous, transparent, then unknown"},
}

// SortKeys lists the accepted Filter.Sort values, sorted.
func SortKeys() []string {
	out := make([]string, 0, len(sortKeys))
	for k := range sortKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SortKeyHelp describes a sort key.
func SortKeyHelp(key string) string { return sortKeys[key].help }

// ExpandSchemes resolves scheme groups: "socks" = socks4, socks4a, socks5;
// "http" stays exact unless httpGroup is set (then it also means https, as
// route's -H does). Values are lower-cased and de-duplicated.
func ExpandSchemes(in []string, httpGroup bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, v := range in {
		for _, s := range strings.Split(v, ",") {
			s = strings.ToLower(strings.TrimSpace(s))
			switch {
			case s == "" || s == "any" || s == "all":
			case s == "socks":
				add("socks4")
				add("socks4a")
				add("socks5")
			case s == "http" && httpGroup:
				add("http")
				add("https")
			default:
				add(s)
			}
		}
	}
	return out
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	in := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		conds = append(conds, col+" IN ("+placeholders(len(vals))+")")
		for _, v := range vals {
			args = append(args, v)
		}
	}
	if len(f.IDs) > 0 {
		conds = append(conds, "p.id IN ("+placeholders(len(f.IDs))+")")
		for _, v := range f.IDs {
			args = append(args, v)
		}
	}
	in("p.status", f.Status)
	in("p.scheme", f.Schemes)
	var countries []string
	for _, c := range f.Countries {
		countries = append(countries, strings.ToUpper(c))
	}
	in("upper(p.country)", countries)

	if f.MinMS > 0 || f.MaxMS > 0 {
		var ms []string
		if f.MinMS > 0 {
			ms = append(ms, "p.last_ms >= ?")
			args = append(args, f.MinMS)
		}
		if f.MaxMS > 0 {
			ms = append(ms, "p.last_ms <= ?")
			args = append(args, f.MaxMS)
		}
		cond := "(" + strings.Join(ms, " AND ") + ")"
		if f.IncludeUntimed && f.MinMS == 0 {
			cond = "(p.last_ms IS NULL OR " + cond + ")"
		}
		conds = append(conds, cond)
	}
	if f.Tag != "" {
		c := "EXISTS (SELECT 1 FROM tags t WHERE t.proxy_id = p.id AND t.tag = ?"
		args = append(args, f.Tag)
		if f.TagScope != "" {
			c += " AND t.scope = ?"
			args = append(args, f.TagScope)
		}
		conds = append(conds, c+")")
	}
	if f.ListedMax != nil {
		conds = append(conds, "p.listed_count <= ?")
		args = append(args, *f.ListedMax)
	}
	if f.MinOKStreak > 0 {
		conds = append(conds, "p.ok_streak >= ?")
		args = append(args, f.MinOKStreak)
	}
	if f.Source != "" {
		conds = append(conds, "p.source = ?")
		args = append(args, f.Source)
	}
	if !f.CheckedSince.IsZero() {
		conds = append(conds, "p.checked_at >= ?")
		args = append(args, f.CheckedSince.UnixMilli())
	}
	if len(f.ExcludeTags) > 0 {
		conds = append(conds, "NOT EXISTS (SELECT 1 FROM tags t WHERE t.proxy_id = p.id AND t.tag IN ("+
			placeholders(len(f.ExcludeTags))+"))")
		for _, t := range f.ExcludeTags {
			args = append(args, t)
		}
	}
	in("p.anonymity", f.Anonymity)
	if f.HTTPSOnly {
		conds = append(conds, "p.https_ok = 1")
	}
	if f.AfterID > 0 {
		conds = append(conds, "p.id > ?")
		args = append(args, f.AfterID)
	}
	if !f.FullScanBefore.IsZero() {
		conds = append(conds, "(p.full_scan_at IS NULL OR p.full_scan_at < ?)")
		args = append(args, f.FullScanBefore.UnixMilli())
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(s)) + "%"
		conds = append(conds, `(p.host LIKE ? ESCAPE '\'
			OR (p.scheme || '://' || p.host || ':' || p.port) LIKE ? ESCAPE '\'
			OR lower(coalesce(p.exit_ip, '')) LIKE ? ESCAPE '\'
			OR lower(coalesce(p.country, '')) LIKE ? ESCAPE '\'
			OR lower(coalesce(p.org, '')) LIKE ? ESCAPE '\'
			OR lower(p.source) LIKE ? ESCAPE '\')`)
		for i := 0; i < 6; i++ {
			args = append(args, like)
		}
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (f Filter) orderBy() (string, error) {
	key := f.Sort
	if key == "" {
		key = "id"
	}
	spec, ok := sortKeys[key]
	if !ok {
		return "", fmt.Errorf("db: unknown sort key %q (want one of: %s)", key, strings.Join(SortKeys(), ", "))
	}
	if key == "random" {
		return " ORDER BY random()", nil
	}
	dir := "ASC"
	if spec.desc != f.Reverse {
		dir = "DESC"
	}
	return fmt.Sprintf(" ORDER BY (%s IS NULL), %s %s, p.id ASC", spec.expr, spec.expr, dir), nil
}
