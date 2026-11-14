package score

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	t.Setenv("SILKROUTER_HOME", t.TempDir())
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func addProxy(t *testing.T, d *db.DB, url string) int64 {
	t.Helper()
	a, err := db.ParseAddr(url)
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.UpsertProxy(context.Background(), a, "test")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func measure(t *testing.T, d *db.DB, id int64, ok bool, ms int, n int) {
	t.Helper()
	var batch []db.Measurement
	for i := 0; i < n; i++ {
		batch = append(batch, db.Measurement{ProxyID: id, Kind: db.KindScan, Probe: "cloudflare", OK: ok, MS: ms})
	}
	if err := d.RecordMeasurements(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
}

func use(t *testing.T, d *db.DB, id int64, url, cmd string, exit, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		code := exit
		if _, err := d.RecordUsage(context.Background(), db.Usage{ProxyID: id, ProxyURL: url, Argv: []string{cmd, "x"},
			ExitCode: &code, TSEnd: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestComputeAndTags(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	fast := addProxy(t, d, "socks5://192.0.2.1:1080")
	slow := addProxy(t, d, "http://192.0.2.2:8080")
	dirty := addProxy(t, d, "http://192.0.2.3:3128")
	dead := addProxy(t, d, "http://192.0.2.4:80")
	flaky := addProxy(t, d, "socks4://192.0.2.5:1080")

	measure(t, d, fast, true, 40, 12)
	measure(t, d, slow, true, 900, 12)
	measure(t, d, dirty, true, 60, 12)
	measure(t, d, dead, false, 0, 3)
	measure(t, d, flaky, false, 0, 8)
	measure(t, d, flaky, true, 300, 3)

	if err := d.UpsertReputation(ctx,
		db.Reputation{IP: "192.0.2.3", ListName: "zen.spamhaus.org", ListKind: "dnsbl", Listed: true},
		db.Reputation{IP: "192.0.2.3", ListName: "tor-exits", ListKind: "torexit", Listed: true},
		db.Reputation{IP: "192.0.2.1", ListName: "zen.spamhaus.org", ListKind: "dnsbl", Listed: false},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := d.SetInfo(ctx,
		db.ProxyInfo{ProxyID: fast, Anonymity: "elite", HTTPSOK: db.Bool(true), FullScanAt: now},
		db.ProxyInfo{ProxyID: dirty, Anonymity: "transparent", FullScanAt: now},
	); err != nil {
		t.Fatal(err)
	}
	use(t, d, fast, "socks5://192.0.2.1:1080", "curl", 0, 11)
	use(t, d, slow, "http://192.0.2.2:8080", "curl", 0, 3)
	use(t, d, slow, "http://192.0.2.2:8080", "git", 1, 4)

	res, err := Compute(ctx, d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scored != 5 || res.Ranked != 4 {
		t.Fatalf("%+v", res)
	}
	get := func(id int64) *db.Proxy {
		p, err := d.GetProxy(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	pf, ps, pd, px := get(fast), get(slow), get(dirty), get(dead)
	if pf.Rank == nil || *pf.Rank != 1 {
		t.Fatalf("fast clean proxy should rank first: %v", pf.Rank)
	}
	if px.Rank != nil {
		t.Fatal("dead proxy ranked")
	}
	if *pf.SpeedScore <= *ps.SpeedScore || *pd.CleanScore >= *pf.CleanScore {
		t.Fatalf("scores: fast %v/%v slow %v dirty clean %v", *pf.SpeedScore, *pf.CleanScore, *ps.SpeedScore, *pd.CleanScore)
	}
	if *pd.CleanScore > 20 {
		t.Fatalf("dnsbl + tor + transparent should be dirty: %v", *pd.CleanScore)
	}
	if ps.CleanScore != nil {
		t.Fatal("never checked proxy got a clean score")
	}

	// Speed / clean modes reorder.
	if _, err := Compute(ctx, d, Options{Mode: RankClean}); err != nil {
		t.Fatal(err)
	}
	if r := get(dirty).Rank; r == nil || *r == 1 {
		t.Fatal("clean rank put the dirty proxy first")
	}

	tr, err := RefreshTags(ctx, d, TagOptions{RecommendTop: 2})
	if err != nil {
		t.Fatal(err)
	}
	labels := func(id int64) string {
		ts, _ := d.Tags(ctx, id)
		var ls []string
		for _, t := range ts {
			ls = append(ls, t.Label())
		}
		return strings.Join(ls, " ")
	}
	lf := labels(fast)
	for _, want := range []string{"stable", "fast", "fav:curl", "clean", "elite", "https", "recommended", "recommended:curl"} {
		if !strings.Contains(" "+lf+" ", " "+want+" ") {
			t.Errorf("fast proxy lacks %q: %s", want, lf)
		}
	}
	ls := labels(slow)
	if strings.Contains(ls, "fast") || strings.Contains(ls, "fav:") || !strings.Contains(ls, "stable") {
		t.Errorf("slow proxy tags: %s", ls)
	}
	if ld := labels(dirty); !strings.Contains(ld, "tor") || strings.Contains(ld, "clean") {
		t.Errorf("dirty proxy tags: %s", ld)
	}
	if lfl := labels(flaky); !strings.Contains(lfl, "flaky") {
		t.Errorf("flaky proxy tags: %s", lfl)
	}
	if labels(dead) != "" {
		t.Errorf("dead proxy tags: %s", labels(dead))
	}
	if tr.Counts["fast"] != 2 || tr.Total == 0 { // fast (40ms) and dirty (60ms)
		t.Fatalf("%+v", tr)
	}

	// Manual tags survive a refresh; heuristic ones keep their since.
	if err := d.SetTag(ctx, db.Tag{ProxyID: slow, Tag: "blocked"}); err != nil {
		t.Fatal(err)
	}
	before, _ := d.Tags(ctx, fast)
	time.Sleep(5 * time.Millisecond)
	if _, err := RefreshTags(ctx, d, TagOptions{RecommendTop: 2}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(labels(slow), "blocked") {
		t.Fatal("manual tag dropped")
	}
	after, _ := d.Tags(ctx, fast)
	if len(after) != len(before) || !after[0].Since.Equal(before[0].Since) {
		t.Fatal("since not kept")
	}

	// Incremental refresh: the slow proxy turns fast after 10 quick checks.
	measure(t, d, slow, true, 50, 10)
	if err := RefreshFor(ctx, d, TagOptions{RecommendTop: 2}, slow, "curl"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(labels(slow), "fast") || !strings.Contains(labels(slow), "blocked") {
		t.Fatalf("incremental: %s", labels(slow))
	}
	if !strings.Contains(labels(fast), "recommended:curl") {
		t.Fatalf("incremental refresh dropped others' tags: %s", labels(fast))
	}
	if !IsHeuristic("fav") || IsHeuristic("blocked") {
		t.Fatal("IsHeuristic")
	}
}
