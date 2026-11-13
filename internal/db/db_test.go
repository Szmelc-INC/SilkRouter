package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/source"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	t.Setenv("SILKROUTER_HOME", t.TempDir())
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func mustAddr(t *testing.T, s string) Addr {
	t.Helper()
	a, err := ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustUpsert(t *testing.T, d *DB, s string) int64 {
	t.Helper()
	id, err := d.UpsertProxy(context.Background(), mustAddr(t, s), "test")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOpenPragmasAndMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	var fk, busy, sync int
	d.SQL().QueryRow("PRAGMA journal_mode").Scan(&mode)
	d.SQL().QueryRow("PRAGMA foreign_keys").Scan(&fk)
	d.SQL().QueryRow("PRAGMA busy_timeout").Scan(&busy)
	d.SQL().QueryRow("PRAGMA synchronous").Scan(&sync)
	if mode != "wal" || fk != 1 || busy != 5000 || sync != 1 {
		t.Fatalf("pragmas: journal=%s fk=%d busy=%d sync=%d", mode, fk, busy, sync)
	}
	if v, _ := d.SchemaVersion(ctx); v != LatestVersion() {
		t.Fatalf("version %d, want %d", v, LatestVersion())
	}
	// Idempotent: re-running and re-opening applies nothing.
	if n, err := d.Migrate(ctx); n != 0 || err != nil {
		t.Fatalf("second migrate: %d %v", n, err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	infos, _ := d.Migrations(ctx)
	if len(infos) != len(migrations) || !infos[0].Applied {
		t.Fatalf("migrations: %+v", infos)
	}
	var rows int
	d.SQL().QueryRow("SELECT count(*) FROM schema_version").Scan(&rows)
	if rows != len(migrations) {
		t.Fatalf("schema_version rows %d", rows)
	}
	for _, tbl := range []string{"proxies", "measurements", "reputation", "usage_log", "tags", "scan_runs", "meta"} {
		var n int
		d.SQL().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&n)
		if n != 1 {
			t.Errorf("table %s missing", tbl)
		}
	}
}

func TestNoMigrateAndConcurrentOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	raw, err := OpenWith(path, Options{NoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := raw.SchemaVersion(context.Background()); v != 0 {
		t.Fatalf("NoMigrate applied migrations: %d", v)
	}
	raw.Close()
	// Several "processes" opening a fresh file at once must not collide.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			d.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestUpsertDedup(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	a := mustUpsert(t, d, "socks5://1.2.3.4:1080")
	b := mustUpsert(t, d, "socks5://1.2.3.4:1080 [99ms]")
	c := mustUpsert(t, d, "http://1.2.3.4:1080")
	if a != b || a == c {
		t.Fatalf("ids %d %d %d", a, b, c)
	}
	// Credentials are kept and the first source sticks.
	id, _ := d.UpsertProxy(ctx, mustAddr(t, "socks5://u:p@1.2.3.4:1080"), "other")
	p, err := d.GetProxy(ctx, id)
	if err != nil || id != a || p.Username != "u" || p.Password != "p" || p.Source != "test" {
		t.Fatalf("%+v %v", p, err)
	}
	if p.URL() != "socks5://u:p@1.2.3.4:1080" || p.Key() != "socks5://1.2.3.4:1080" {
		t.Fatal(p.URL(), p.Key())
	}
	if _, err := d.GetProxyByURL(ctx, "socks5://1.2.3.4:1080"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetProxy(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// FindProxy: id, #id, URL, host:port, ambiguous host.
	for _, ref := range []string{fmt.Sprint(a), fmt.Sprintf("#%d", a), "socks5://1.2.3.4:1080"} {
		if p, err := d.FindProxy(ctx, ref); err != nil || p.ID != a {
			t.Fatalf("FindProxy(%q): %v %v", ref, p, err)
		}
	}
	if _, err := d.FindProxy(ctx, "1.2.3.4:1080"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := d.FindProxy(ctx, "9.9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	res, err := d.ImportProxies(ctx, []proxy.Proxy{
		{Scheme: "socks5", Host: "1.2.3.4", Port: 1080},
		{Scheme: "http", Host: "5.6.7.8", Port: 80, LatencyMS: 50},
		{Scheme: "http", Host: "5.6.7.8", Port: 80},
	}, ImportOptions{Source: "x", Latencies: true})
	if err != nil || res.Added != 1 || res.Existing != 1 || res.Measurements != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if n, _ := d.CountProxies(ctx, Filter{}); n != 3 {
		t.Fatalf("count %d", n)
	}
}

func TestRecordMeasurementSummary(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	id := mustUpsert(t, d, "socks5://10.0.0.1:1080")
	t0 := time.Unix(1_700_000_000, 0)
	steps := []struct {
		ok bool
		ms int
		ip string
	}{{true, 100, "8.8.8.8"}, {true, 300, ""}, {false, 0, ""}, {false, 0, ""}, {true, 50, "9.9.9.9"}, {true, 0, ""}}
	for i, s := range steps {
		if _, err := d.RecordMeasurement(ctx, Measurement{ProxyID: id, TS: t0.Add(time.Duration(i) * time.Second),
			Kind: KindProbe, Probe: "x", OK: s.ok, MS: s.ms, ExitIP: s.ip, ErrClass: map[bool]string{false: ErrTimeout}[s.ok]}); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := d.GetProxy(ctx, id)
	want := Proxy{Status: StatusAlive, LastMS: 50, BestMS: 50, AvgMS: 150, MSCount: 3, MSSum: 450,
		OKCount: 4, FailCount: 2, OKStreak: 2, FailStreak: 0, ExitIP: "9.9.9.9"}
	got := Proxy{Status: p.Status, LastMS: p.LastMS, BestMS: p.BestMS, AvgMS: p.AvgMS, MSCount: p.MSCount, MSSum: p.MSSum,
		OKCount: p.OKCount, FailCount: p.FailCount, OKStreak: p.OKStreak, FailStreak: p.FailStreak, ExitIP: p.ExitIP}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary\n got %+v\nwant %+v", got, want)
	}
	if !p.CheckedAt.Equal(t0.Add(5 * time.Second)) {
		t.Fatalf("checked_at %v", p.CheckedAt)
	}
	// A failure flips status and streaks; last/best/avg stay.
	d.RecordMeasurement(ctx, Measurement{ProxyID: id, Kind: KindScan, OK: false})
	p, _ = d.GetProxy(ctx, id)
	if p.Status != StatusDead || p.OKStreak != 0 || p.FailStreak != 1 || p.LastMS != 50 || p.AvgMS != 150 {
		t.Fatalf("after failure: %+v", p)
	}
	ms, _ := d.Measurements(ctx, id, 3)
	if len(ms) != 3 || ms[0].Kind != KindScan || ms[1].MS != 0 || ms[2].MS != 50 {
		t.Fatalf("recent: %+v", ms)
	}
	// Unknown proxies are created from Addr.
	if _, err := d.RecordMeasurement(ctx, Measurement{Addr: mustAddr(t, "http://7.7.7.7:8080"), Kind: KindRoute, OK: true, MS: 70}); err != nil {
		t.Fatal(err)
	}
	if p, err := d.GetProxyByURL(ctx, "http://7.7.7.7:8080"); err != nil || p.LastMS != 70 || p.Status != StatusAlive {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := d.RecordMeasurement(ctx, Measurement{ProxyID: id, Kind: "bogus"}); err == nil {
		t.Fatal("bad kind accepted")
	}
}

func seedList(t *testing.T, d *DB) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	ids := map[string]int64{}
	data := []struct {
		url string
		ms  []int // 0 = failure
	}{
		{"socks5://1.1.1.1:1080", []int{40, 60}},
		{"socks4://2.2.2.2:1080", []int{900}},
		{"http://3.3.3.3:80", []int{200, 0}},
		{"https://4.4.4.4:443", []int{1500}},
		{"http://5.5.5.5:8080", nil},
		{"socks5://6.6.6.6:1080", []int{90, 80, 70}},
	}
	for _, e := range data {
		id := mustUpsert(t, d, e.url)
		ids[e.url] = id
		for _, m := range e.ms {
			d.RecordMeasurement(ctx, Measurement{ProxyID: id, Kind: KindProbe, OK: m > 0, MS: m})
		}
	}
	return ids
}

func urls(ps []*Proxy) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Host)
	}
	return out
}

func TestListProxiesFilters(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	ids := seedList(t, d)
	d.SetTag(ctx, Tag{ProxyID: ids["socks5://6.6.6.6:1080"], Tag: "fast"})
	d.SetTag(ctx, Tag{ProxyID: ids["socks5://1.1.1.1:1080"], Tag: "fav", Scope: "curl"})
	d.SetGeo(ctx, ids["socks4://2.2.2.2:1080"], "PL", "AS1", "Org", "elite")
	d.UpsertReputation(ctx, Reputation{IP: "1.1.1.1", ListName: "spamhaus", ListKind: "dnsbl", Listed: true})
	two := 2
	d.SetScores(ctx, []SetScore{{ProxyID: ids["socks5://6.6.6.6:1080"], Rank: new(1)}, {ProxyID: ids["socks5://1.1.1.1:1080"], Rank: &two}})

	cases := []struct {
		name string
		f    Filter
		want string
	}{
		{"all by id", Filter{}, "1.1.1.1 2.2.2.2 3.3.3.3 4.4.4.4 5.5.5.5 6.6.6.6"},
		{"alive by last_ms", Filter{Status: []string{StatusAlive}, Sort: "last_ms"}, "1.1.1.1 6.6.6.6 2.2.2.2 4.4.4.4"},
		{"reverse", Filter{Status: []string{StatusAlive}, Sort: "last_ms", Reverse: true}, "4.4.4.4 2.2.2.2 6.6.6.6 1.1.1.1"},
		{"best_ms", Filter{Sort: "best_ms", Limit: 2}, "1.1.1.1 6.6.6.6"},
		{"avg_ms nulls last", Filter{Sort: "avg_ms"}, "1.1.1.1 6.6.6.6 3.3.3.3 2.2.2.2 4.4.4.4 5.5.5.5"},
		{"max ms", Filter{MaxMS: 100}, "1.1.1.1 6.6.6.6"},
		{"min+max", Filter{MinMS: 61, MaxMS: 1000}, "2.2.2.2 3.3.3.3 6.6.6.6"},
		{"untimed passes", Filter{MaxMS: 100, IncludeUntimed: true}, "1.1.1.1 5.5.5.5 6.6.6.6"},
		{"socks", Filter{Schemes: ExpandSchemes([]string{"socks"}, false)}, "1.1.1.1 2.2.2.2 6.6.6.6"},
		{"http group", Filter{Schemes: ExpandSchemes([]string{"http"}, true)}, "3.3.3.3 4.4.4.4 5.5.5.5"},
		{"http exact", Filter{Schemes: ExpandSchemes([]string{"http"}, false)}, "3.3.3.3 5.5.5.5"},
		{"tag", Filter{Tag: "fast"}, "6.6.6.6"},
		{"tag scope", Filter{Tag: "fav", TagScope: "curl"}, "1.1.1.1"},
		{"tag wrong scope", Filter{Tag: "fav", TagScope: "git"}, ""},
		{"country", Filter{Countries: []string{"pl"}}, "2.2.2.2"},
		{"listed max", Filter{ListedMax: new(0), Status: []string{StatusAlive}}, "2.2.2.2 4.4.4.4 6.6.6.6"},
		{"search", Filter{Search: "socks4://"}, "2.2.2.2"},
		{"search org", Filter{Search: "org"}, "2.2.2.2"},
		{"rank", Filter{Sort: "rank", Limit: 3}, "6.6.6.6 1.1.1.1 2.2.2.2"},
		{"ok_streak", Filter{Sort: "ok_streak", Limit: 2}, "6.6.6.6 1.1.1.1"},
		{"offset", Filter{Limit: 2, Offset: 2}, "3.3.3.3 4.4.4.4"},
		{"offset no limit", Filter{Offset: 4}, "5.5.5.5 6.6.6.6"},
		{"streak min", Filter{MinOKStreak: 2}, "1.1.1.1 6.6.6.6"},
		{"dead", Filter{Status: []string{StatusDead}}, "3.3.3.3"},
		{"unknown", Filter{Status: []string{StatusUnknown}}, "5.5.5.5"},
	}
	for _, c := range cases {
		ps, err := d.ListProxies(ctx, c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := strings.Join(urls(ps), " "); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
		if n, _ := d.CountProxies(ctx, Filter{Status: c.f.Status, Schemes: c.f.Schemes, MaxMS: c.f.MaxMS, MinMS: c.f.MinMS,
			Tag: c.f.Tag, TagScope: c.f.TagScope}); c.f.Limit == 0 && c.f.Offset == 0 && c.f.Countries == nil &&
			c.f.ListedMax == nil && c.f.Search == "" && c.f.MinOKStreak == 0 && !c.f.IncludeUntimed && n != len(ps) {
			t.Errorf("%s: count %d != %d", c.name, n, len(ps))
		}
	}
	ps, _ := d.ListProxies(ctx, Filter{WithTags: true, IDs: []int64{ids["socks5://1.1.1.1:1080"]}})
	if len(ps) != 1 || !reflect.DeepEqual(ps[0].Tags, []string{"fav:curl"}) || ps[0].ListedCount != 1 || *ps[0].Rank != 2 {
		t.Fatalf("tags/listed: %+v", ps[0])
	}
	if _, err := d.ListProxies(ctx, Filter{Sort: "nope"}); err == nil {
		t.Fatal("bad sort accepted")
	}
	if ps, _ := d.ListProxies(ctx, Filter{Sort: "random"}); len(ps) != 6 {
		t.Fatal("random")
	}
}

func TestReputationUsageTagsScanMeta(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	id := mustUpsert(t, d, "socks5://1.2.3.4:1080")
	if err := d.UpsertReputation(ctx,
		Reputation{IP: "1.2.3.4", ListName: "a", ListKind: "dnsbl", Listed: true},
		Reputation{IP: "1.2.3.4", ListName: "b", ListKind: "ipset", Listed: false},
		Reputation{IP: "1.2.3.4", ListName: "c", ListKind: "torexit", Listed: true}); err != nil {
		t.Fatal(err)
	}
	p, _ := d.GetProxy(ctx, id)
	if p.ListedCount != 2 {
		t.Fatalf("listed %d", p.ListedCount)
	}
	d.UpsertReputation(ctx, Reputation{IP: "1.2.3.4", ListName: "a", ListKind: "dnsbl", Listed: false})
	if p, _ = d.GetProxy(ctx, id); p.ListedCount != 1 {
		t.Fatalf("listed after delist %d", p.ListedCount)
	}
	if rs, _ := d.ReputationFor(ctx, "1.2.3.4"); len(rs) != 3 || !rs[0].Listed {
		t.Fatalf("%+v", rs)
	}
	if err := d.UpsertReputation(ctx, Reputation{IP: "x", ListName: "y", ListKind: "bogus"}); err == nil {
		t.Fatal("bad kind accepted")
	}

	uid, err := d.StartUsage(ctx, Usage{ProxyID: id, ProxyURL: p.URL(), Argv: []string{"/usr/bin/curl", "-s", "x"}, Cwd: "/tmp", PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	d.SetUsagePreflight(ctx, uid, true, 120)
	if err := d.FinishUsage(ctx, uid, 3, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	us, _ := d.ListUsage(ctx, UsageFilter{ProxyID: id})
	if len(us) != 1 || us[0].Cmd != "curl" || *us[0].ExitCode != 3 || us[0].ErrClass != ErrExitNonzero ||
		!*us[0].PreflightOK || *us[0].PreflightMS != 120 || len(us[0].Argv) != 3 || us[0].Mode != ModeRoute {
		t.Fatalf("%+v", us)
	}
	if n, _ := d.UsageCount(ctx, id); n != 1 {
		t.Fatal(n)
	}
	if err := d.FinishUsage(ctx, 999, 0, "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	d.SetTags(ctx, []Tag{{ProxyID: id, Tag: "stable", Reason: "10/10"}, {ProxyID: id, Tag: "fav", Scope: "git"}})
	tags, _ := d.Tags(ctx, id)
	if len(tags) != 2 || tags[0].Label() != "fav:git" || tags[1].Reason != "10/10" {
		t.Fatalf("%+v", tags)
	}
	since := tags[1].Since
	if err := d.ReplaceTags(ctx, []string{"stable"}, []Tag{{ProxyID: id, Tag: "stable", Reason: "again"}}); err != nil {
		t.Fatal(err)
	}
	tags, _ = d.Tags(ctx, id)
	if !tags[1].Since.Equal(since) || tags[1].Reason != "again" {
		t.Fatalf("replace kept since? %+v", tags)
	}
	if tc, _ := d.TagCounts(ctx); len(tc) != 2 {
		t.Fatalf("%+v", tc)
	}
	d.ClearTag(ctx, id, "fav", "git")
	if n, _ := d.ClearTags(ctx, "stable", "*"); n != 1 {
		t.Fatal(n)
	}
	if tags, _ = d.Tags(ctx, 0); len(tags) != 0 {
		t.Fatalf("%+v", tags)
	}

	rid, err := d.CreateScanRun(ctx, ScanRun{Mode: "full", Total: 100, Opts: `{"x":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateScanRun(ctx, rid, 40, "cursor-40", ScanPaused, 0); err != nil {
		t.Fatal(err)
	}
	r, err := d.ResumableScanRun(ctx, "full")
	if err != nil || r.ID != rid || r.Done != 40 || r.Cursor != "cursor-40" || r.Total != 100 || r.State != ScanPaused {
		t.Fatalf("%+v %v", r, err)
	}
	d.UpdateScanRun(ctx, rid, 100, "", ScanDone, 0)
	if _, err := d.ResumableScanRun(ctx, "full"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if r, _ = d.GetScanRun(ctx, rid); r.FinishedAt.IsZero() || r.State != ScanDone {
		t.Fatalf("%+v", r)
	}
	if runs, _ := d.ListScanRuns(ctx, 5); len(runs) != 1 {
		t.Fatal(runs)
	}

	if _, ok, _ := d.GetMeta(ctx, "k"); ok {
		t.Fatal("meta set")
	}
	d.SetMeta(ctx, "k", "v1")
	d.SetMeta(ctx, "k", "v2")
	if v, ok, _ := d.GetMeta(ctx, "k"); !ok || v != "v2" {
		t.Fatal(v)
	}
	// Deleting a proxy cascades to facts and tags; usage keeps a NULL ref.
	d.RecordMeasurement(ctx, Measurement{ProxyID: id, OK: true, MS: 5})
	d.SetTag(ctx, Tag{ProxyID: id, Tag: "x"})
	if err := d.DeleteProxy(ctx, id); err != nil {
		t.Fatal(err)
	}
	s, err := d.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Proxies != 0 || s.Measurements != 0 || s.Tags != 0 || s.Usage != 1 || s.Reputation != 3 {
		t.Fatalf("%+v", s)
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	seedList(t, d)
	s, err := d.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Proxies != 6 || s.ByStatus[StatusAlive] != 4 || s.ByStatus[StatusDead] != 1 || s.ByStatus[StatusUnknown] != 1 ||
		s.ByScheme["socks5"] != 2 || s.Measured != 5 || s.Measurements != 9 || s.BestMS != 60 ||
		s.Latency.Under100 != 2 || s.Latency.To1s != 3 || s.Latency.To5s != 1 || s.SchemaVersion != LatestVersion() {
		t.Fatalf("%+v", s)
	}
	if s.MedianMS != 70 { // alive last_ms: 60 70 900 1500 -> lower median 70
		t.Fatalf("median %d", s.MedianMS)
	}
}

func snapshot(t *testing.T, d *DB) []Proxy {
	t.Helper()
	ps, err := d.ListProxies(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]Proxy, len(ps))
	for i, p := range ps {
		out[i] = *p
		out[i].AvgMS = math.Round(p.AvgMS*1e6) / 1e6
	}
	return out
}

func TestRebuildStatsEqualsIncremental(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	seedList(t, d)
	d.UpsertReputation(ctx, Reputation{IP: "1.1.1.1", ListName: "l", ListKind: "dnsbl", Listed: true})
	// Interleave proxies inside one batch (Writer path) too.
	w := d.NewWriter(ctx, WriterOptions{BatchSize: 7})
	for i := 0; i < 50; i++ {
		w.Measure(Measurement{Addr: mustAddr(t, fmt.Sprintf("socks5://10.0.0.%d:1080", i%5)), OK: i%3 != 0, MS: 10 + i, ExitIP: fmt.Sprint(i % 2)})
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, d)
	// Scramble the summaries, then rebuild.
	d.SQL().Exec(`UPDATE proxies SET status='dead', last_ms=1, ok_streak=99, avg_ms=3, listed_count=7`)
	res, err := d.RebuildStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Measurements != 59 || res.Proxies != 10 {
		t.Fatalf("%+v", res)
	}
	after := snapshot(t, d)
	if !reflect.DeepEqual(before, after) {
		for i := range before {
			if !reflect.DeepEqual(before[i], after[i]) {
				t.Errorf("proxy %d\nincremental %+v\n    rebuilt %+v", i, before[i], after[i])
			}
		}
	}
}

func TestWriterConcurrency(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	const workers, perWorker = 50, 200
	var mu sync.Mutex
	var errs []error
	w := d.NewWriter(ctx, WriterOptions{BatchSize: 250, FlushInterval: 20 * time.Millisecond,
		OnError: func(err error, _ int) { mu.Lock(); errs = append(errs, err); mu.Unlock() }})
	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				// 100 distinct proxies shared across workers.
				a := Addr{Scheme: "socks5", Host: fmt.Sprintf("10.1.%d.%d", (g*perWorker+i)%100/10, (g*perWorker+i)%10), Port: 1080}
				if err := w.Measure(Measurement{Addr: a, Kind: KindScan, OK: i%4 != 0, MS: 1 + i%50}); err != nil {
					t.Error(err)
					return
				}
				if i%50 == 0 {
					w.Reputation(Reputation{IP: a.Host, ListName: fmt.Sprintf("l%d", g), ListKind: "dnsbl", Listed: true})
				}
				if i == perWorker/2 && g%10 == 0 {
					if err := w.Flush(); err != nil {
						t.Error(err)
					}
				}
			}
		}(g)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	st := w.Stats()
	if st.Measurements != workers*perWorker || st.Failed != 0 {
		t.Fatalf("%+v", st)
	}
	s, _ := d.Stats(ctx)
	if s.Measurements != workers*perWorker || s.Proxies != 100 {
		t.Fatalf("%+v", s)
	}
	var okSum, failSum int
	d.SQL().QueryRow(`SELECT sum(ok_count), sum(fail_count) FROM proxies`).Scan(&okSum, &failSum)
	if okSum+failSum != workers*perWorker || failSum != workers*perWorker/4 {
		t.Fatalf("ok %d fail %d", okSum, failSum)
	}
	t.Logf("writer: %d rows in %d batches, %v (%.0f rows/s)", st.Measurements+st.Reputation, st.Batches, elapsed,
		float64(st.Measurements)/elapsed.Seconds())
	if err := w.Measure(Measurement{}); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("send after close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("second close:", err)
	}
}

func TestWriterCancelKeepsPending(t *testing.T) {
	d := openTemp(t)
	ctx, cancel := context.WithCancel(context.Background())
	w := d.NewWriter(ctx, WriterOptions{BatchSize: 1000, FlushInterval: time.Hour})
	for i := 0; i < 30; i++ {
		if err := w.Measure(Measurement{Addr: Addr{Scheme: "http", Host: "1.1.1.1", Port: 80 + i}, OK: true, MS: 5}); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	<-w.Done()
	if err := w.Measure(Measurement{}); !errors.Is(err, ErrWriterClosed) {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal("flush after cancel:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.CountProxies(context.Background(), Filter{Status: []string{StatusAlive}}); n != 30 {
		t.Fatalf("pending rows lost on cancel: %d", n)
	}
}

func TestWriterReportsErrors(t *testing.T) {
	d := openTemp(t)
	var got int
	w := d.NewWriter(context.Background(), WriterOptions{OnError: func(_ error, n int) { got += n }})
	w.Measure(Measurement{ProxyID: 12345, OK: true}) // FK violation
	if err := w.Flush(); err == nil {
		t.Fatal("flush hid the error")
	}
	if err := w.Close(); err == nil || got != 1 || w.Stats().Failed != 1 {
		t.Fatalf("close: %v got=%d", err, got)
	}
}

func readBuiltin(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "proxy", "LIST.txt"))
	if err != nil {
		t.Skip("embedded list not found:", err)
	}
	return data
}

func TestImportRealList(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	data := readBuiltin(t)
	unique := len(source.ParseLines(data))
	cacheFile := filepath.Join(t.TempDir(), "cache.list")
	os.WriteFile(cacheFile, []byte("socks5://208.102.51.6:58208 [120ms]\nhttp://203.0.113.9:3128 [80ms]\nhttp://203.0.113.10:3128\n"), 0o600)

	res, err := d.FirstRun(ctx, FirstRunOptions{Builtin: data, CacheFile: cacheFile})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first run: builtin %d (+%d) cache %d in %v", res.Builtin.Total, res.Builtin.Added, res.Cache.Total, res.Elapsed)
	if !res.Ran || res.Builtin.Added != unique || res.Cache.Total != 3 || res.Cache.Added != 2 || res.Cache.Measurements != 3 {
		t.Fatalf("%+v (unique lines %d)", res, unique)
	}
	if unique < 11000 {
		t.Fatalf("only %d unique proxies in the embedded list", unique)
	}
	n, _ := d.CountProxies(ctx, Filter{})
	if n != unique+2 {
		t.Fatalf("count %d, want %d", n, unique+2)
	}
	alive, _ := d.ListProxies(ctx, Filter{Status: []string{StatusAlive}, Sort: "last_ms"})
	if len(alive) != 3 || alive[0].LastMS != 80 || alive[2].LastMS != 0 || alive[1].Source != SourceBuiltin {
		t.Fatalf("%+v", alive)
	}
	// Second run is a no-op.
	if res, _ = d.FirstRun(ctx, FirstRunOptions{Builtin: data}); res.Ran {
		t.Fatal("first run ran twice")
	}
	// Re-importing the whole list adds nothing.
	ps := source.ParseLines(data)
	r2, err := d.ImportProxies(ctx, ps, ImportOptions{Source: "again"})
	if err != nil || r2.Added != 0 || r2.Existing != unique {
		t.Fatalf("%+v %v", r2, err)
	}
	t.Logf("re-import of %d existing: %v", r2.Total, r2.Elapsed)

	// Query timings on a realistic table.
	for i := 0; i < 2000; i++ {
		d.RecordMeasurement(ctx, Measurement{ProxyID: int64(i + 1), OK: i%3 != 0, MS: 20 + i%900})
	}
	for _, f := range []Filter{
		{Status: []string{StatusAlive}, MaxMS: 500, Schemes: ExpandSchemes([]string{"socks"}, false), Sort: "random", Limit: 1},
		{Sort: "last_ms", Limit: 50, WithTags: true},
		{Search: "208.102", Limit: 50},
	} {
		start := time.Now()
		ps, err := d.ListProxies(ctx, f)
		if err != nil || len(ps) == 0 {
			t.Fatalf("%+v: %v %d", f, err, len(ps))
		}
		t.Logf("ListProxies(%+v): %d rows in %v", f, len(ps), time.Since(start))
	}
	start := time.Now()
	if _, err := d.Stats(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("Stats: %v", time.Since(start))
}

func TestExportRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	seedList(t, d)
	var buf bytes.Buffer
	n, err := d.Export(ctx, &buf, FormatList, Filter{Status: []string{StatusAlive}})
	if err != nil || n != 4 {
		t.Fatal(n, err)
	}
	want := "socks5://1.1.1.1:1080 [60ms]\nsocks5://6.6.6.6:1080 [70ms]\nsocks4://2.2.2.2:1080 [900ms]\nhttps://4.4.4.4:443 [1500ms]\n"
	if buf.String() != want {
		t.Fatalf("export:\n%s", buf.String())
	}
	// Legacy format back into a fresh DB as a cache: same proxies, same ms.
	path := filepath.Join(t.TempDir(), "cache.list")
	os.WriteFile(path, buf.Bytes(), 0o600)
	d2, _ := Open(filepath.Join(t.TempDir(), "2.db"))
	defer d2.Close()
	if _, err := d2.FirstRun(ctx, FirstRunOptions{CacheFile: path}); err != nil {
		t.Fatal(err)
	}
	var buf2 bytes.Buffer
	d2.Export(ctx, &buf2, FormatList, Filter{Status: []string{StatusAlive}})
	if buf2.String() != want {
		t.Fatalf("round trip:\n%s", buf2.String())
	}
	for _, f := range []string{FormatCSV, FormatJSON, FormatURLs} {
		var b bytes.Buffer
		if n, err := d.Export(ctx, &b, f, Filter{}); err != nil || n != 6 || b.Len() == 0 {
			t.Fatalf("%s: %d %v", f, n, err)
		}
	}
	if _, err := d.Export(ctx, &buf, "xml", Filter{}); err == nil {
		t.Fatal("bad format")
	}
}

func TestBackupVacuumRemove(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	seedList(t, d)
	bak := filepath.Join(t.TempDir(), "b.db")
	if err := d.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	if err := d.Backup(ctx, bak); err == nil {
		t.Fatal("backup overwrote")
	}
	if err := d.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := Open(bak)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := b.CountProxies(ctx, Filter{}); n != 6 {
		t.Fatal(n)
	}
	b.Close()
	if err := Remove(bak); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
	ro, err := OpenWith(d.Path(), Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.UpsertProxy(ctx, mustAddr(t, "http://1.1.1.1:1"), ""); err == nil {
		t.Fatal("read-only write succeeded")
	}
}
