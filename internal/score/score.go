// Package score turns what the database knows about every proxy into
// numbers and labels:
//
//   - speed_score (0-100): median of the recent timed successes, penalised
//     by jitter; scan.speed_ref_ms scores 50,
//   - clean_score (0-100): 100 minus penalties for DNS blacklists, known
//     proxy / Tor / abuse / datacenter lists and poor anonymity,
//   - stable_score (0-100): smoothed success rate of the recent facts,
//     blended with the usage success rate,
//   - rank (1 = best) of the alive proxies, by a weighted blend (balanced)
//     or one score,
//   - heuristic tags (tags.go): stable, fast, fav:<cmd>, recommended[:<cmd>],
//     clean, elite, https, flaky, tor, datacenter, known.
//
// Everything is recomputed from the facts, so running it twice gives the
// same result.
package score

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

// Rank orders.
const (
	RankBalanced = "balanced"
	RankSpeed    = "speed"
	RankClean    = "clean"
	RankStable   = "stable"
)

// RankModes lists the rank orders.
var RankModes = []string{RankBalanced, RankSpeed, RankClean, RankStable}

// Options tune Compute.
type Options struct {
	Mode                                   string // RankModes (default balanced)
	WeightSpeed, WeightClean, WeightStable float64
	SpeedRefMS                             int // latency scoring 50 (default 250)
	// Recent is how many facts feed the speed and stability scores.
	Recent int
}

func (o *Options) defaults() {
	if o.Mode == "" {
		o.Mode = RankBalanced
	}
	if o.WeightSpeed+o.WeightClean+o.WeightStable <= 0 {
		o.WeightSpeed, o.WeightClean, o.WeightStable = 0.4, 0.35, 0.25
	}
	if o.SpeedRefMS <= 0 {
		o.SpeedRefMS = 250
	}
	if o.Recent <= 0 {
		o.Recent = 20
	}
}

// Scores of one proxy (nil = not enough data).
type Scores struct {
	ProxyID  int64
	Alive    bool
	Speed    *float64
	Clean    *float64
	Stable   *float64
	Balanced float64
	Rank     int
}

// Result reports Compute.
type Result struct {
	Scored  int           `json:"scored"`
	Ranked  int           `json:"ranked"`
	Elapsed time.Duration `json:"elapsed_ns"`
}

type facts struct {
	alive      bool
	timed      []int
	recentOK   int
	recentN    int
	useCount   int
	useOK      int
	anonymity  string
	httpsOK    sql.NullInt64
	hosting    sql.NullInt64
	mitm       sql.NullInt64
	repChecked bool
	listed     map[string]int // list kind -> distinct lists
}

// Compute recomputes speed / clean / stable scores and the rank of every
// proxy and stores them.
func Compute(ctx context.Context, d *db.DB, opts Options) (Result, error) {
	start := time.Now()
	opts.defaults()
	all, err := load(ctx, d.SQL(), opts.Recent)
	if err != nil {
		return Result{}, err
	}
	scores := make([]*Scores, 0, len(all))
	for id, f := range all {
		scores = append(scores, evaluate(id, f, opts))
	}
	ranked := RankScores(scores, opts.Mode)
	rows := make([]db.SetScore, 0, len(scores))
	for _, s := range scores {
		row := db.SetScore{ProxyID: s.ProxyID, SpeedScore: s.Speed, CleanScore: s.Clean, StableScore: s.Stable}
		if s.Rank > 0 {
			r := s.Rank
			row.Rank = &r
		}
		rows = append(rows, row)
	}
	if err := d.SetScores(ctx, rows, true); err != nil {
		return Result{}, err
	}
	return Result{Scored: len(scores), Ranked: ranked, Elapsed: time.Since(start)}, nil
}

// evaluate scores one proxy.
func evaluate(id int64, f *facts, opts Options) *Scores {
	opts.defaults()
	s := &Scores{ProxyID: id, Alive: f.alive}
	if len(f.timed) > 0 {
		ms := float64(median(f.timed))
		eff := ms + 0.5*stddev(f.timed)
		ref := float64(opts.SpeedRefMS)
		v := round1(100 * ref / (ref + eff))
		s.Speed = &v
	}
	if f.repChecked || f.anonymity != "unknown" || f.httpsOK.Valid || f.mitm.Valid {
		v := clean(f)
		s.Clean = &v
	}
	if f.recentN > 0 || f.useCount > 0 {
		rate := 0.5
		if f.recentN > 0 {
			rate = (float64(f.recentOK) + 1) / (float64(f.recentN) + 2)
		}
		if f.useCount > 0 {
			use := (float64(f.useOK) + 1) / (float64(f.useCount) + 2)
			if f.recentN > 0 {
				rate = 0.7*rate + 0.3*use
			} else {
				rate = use
			}
		}
		v := round1(100 * rate)
		s.Stable = &v
	}
	w := opts.WeightSpeed + opts.WeightClean + opts.WeightStable
	s.Balanced = (opts.WeightSpeed*val(s.Speed, 0) + opts.WeightClean*val(s.Clean, 50) + opts.WeightStable*val(s.Stable, 50)) / w
	return s
}

// clean applies the reputation / anonymity penalties.
func clean(f *facts) float64 {
	v := 100.0
	v -= math.Min(60, 12*float64(f.listed["dnsbl"]))
	v -= math.Min(40, 20*float64(f.listed["proxylist"]))
	if f.listed["torexit"] > 0 {
		v -= 40
	}
	if f.listed["datacenter"] > 0 || (f.hosting.Valid && f.hosting.Int64 != 0) {
		v -= 10
	}
	v -= math.Min(40, 15*float64(f.listed["ipset"]))
	v -= math.Min(30, 15*float64(f.listed["api"]))
	switch f.anonymity {
	case "transparent":
		v -= 40
	case "anonymous":
		v -= 10
	case "elite":
	default:
		v -= 5
	}
	if f.mitm.Valid && f.mitm.Int64 != 0 {
		v -= 50 // it decrypts TLS: never trust it with HTTPS
	}
	if f.httpsOK.Valid && f.httpsOK.Int64 == 0 {
		v -= 5
	}
	return round1(math.Max(0, math.Min(100, v)))
}

// RankScores assigns Rank 1..n to the alive proxies by mode (ties: the
// other scores, then id) and returns n. Dead and unknown proxies get 0.
func RankScores(scores []*Scores, mode string) int {
	var alive []*Scores
	for _, s := range scores {
		s.Rank = 0
		if s.Alive {
			alive = append(alive, s)
		}
	}
	key := func(s *Scores) [4]float64 {
		sp, cl, st := val(s.Speed, -1), val(s.Clean, -1), val(s.Stable, -1)
		switch mode {
		case RankSpeed:
			return [4]float64{sp, s.Balanced, st, cl}
		case RankClean:
			return [4]float64{cl, s.Balanced, sp, st}
		case RankStable:
			return [4]float64{st, s.Balanced, sp, cl}
		default:
			return [4]float64{s.Balanced, sp, st, cl}
		}
	}
	sort.SliceStable(alive, func(i, j int) bool {
		a, b := key(alive[i]), key(alive[j])
		for k := range a {
			if a[k] != b[k] {
				return a[k] > b[k]
			}
		}
		return alive[i].ProxyID < alive[j].ProxyID
	})
	for i, s := range alive {
		s.Rank = i + 1
	}
	return len(alive)
}

// load reads what scoring needs for every proxy.
func load(ctx context.Context, q *sql.DB, recent int) (map[int64]*facts, error) {
	out := map[int64]*facts{}
	rows, err := q.QueryContext(ctx, `SELECT id, status, anonymity, https_ok, hosting, tls_mitm, use_count, use_ok FROM proxies`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		f := &facts{listed: map[string]int{}}
		var id int64
		var status string
		if err := rows.Scan(&id, &status, &f.anonymity, &f.httpsOK, &f.hosting, &f.mitm, &f.useCount, &f.useOK); err != nil {
			rows.Close()
			return nil, err
		}
		f.alive = status == db.StatusAlive
		out[id] = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Recent facts: the last N per proxy (ok count, timed successes).
	rows, err = q.QueryContext(ctx, `SELECT proxy_id, ok, ms FROM (
		SELECT proxy_id, ok, ms, row_number() OVER (PARTITION BY proxy_id ORDER BY id DESC) AS rn
		FROM measurements WHERE `+db.NotAuxCond()+`) WHERE rn <= ?`, recent)
	if err != nil {
		return nil, fmt.Errorf("score: recent facts: %w", err)
	}
	for rows.Next() {
		var id int64
		var ok bool
		var ms sql.NullInt64
		if err := rows.Scan(&id, &ok, &ms); err != nil {
			rows.Close()
			return nil, err
		}
		f := out[id]
		if f == nil {
			continue
		}
		f.recentN++
		if ok {
			f.recentOK++
			if ms.Valid && ms.Int64 > 0 {
				f.timed = append(f.timed, int(ms.Int64))
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reputation per proxy (host or exit IP), by list kind.
	rows, err = q.QueryContext(ctx, `SELECT p.id, r.list_kind, count(DISTINCT CASE WHEN r.listed = 1 THEN r.list_name END)
		FROM proxies p JOIN reputation r ON r.ip = p.host OR r.ip = p.exit_ip
		GROUP BY p.id, r.list_kind`)
	if err != nil {
		return nil, fmt.Errorf("score: reputation: %w", err)
	}
	for rows.Next() {
		var id int64
		var kind string
		var n int
		if err := rows.Scan(&id, &kind, &n); err != nil {
			rows.Close()
			return nil, err
		}
		if f := out[id]; f != nil {
			f.repChecked = true
			f.listed[kind] += n
		}
	}
	rows.Close()
	return out, rows.Err()
}

func val(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func median(xs []int) int {
	s := append([]int(nil), xs...)
	sort.Ints(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2] + 1) / 2
}

func stddev(xs []int) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += float64(x)
	}
	mean := sum / float64(len(xs))
	var v float64
	for _, x := range xs {
		v += (float64(x) - mean) * (float64(x) - mean)
	}
	return math.Sqrt(v / float64(len(xs)-1))
}
