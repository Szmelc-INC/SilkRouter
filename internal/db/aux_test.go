package db

import (
	"context"
	"testing"
)

// Auxiliary facts (https, judge, exit-ip) are stored but do not flip a
// proxy to dead or touch its latency; exit_ip still updates. A rebuild
// gives the same summary.
func TestAuxProbesDoNotDecideLiveness(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	a, _ := ParseAddr("socks5://192.0.2.9:1080")
	facts := []Measurement{
		{Addr: a, Kind: KindScan, Probe: "cloudflare", OK: true, MS: 120},
		{Addr: a, Kind: KindScan, Probe: "google", OK: true, MS: 140},
		{Addr: a, Kind: KindScan, Probe: "exit-ip", OK: true, MS: 900, ExitIP: "203.0.113.9"},
		{Addr: a, Kind: KindScan, Probe: "https", OK: false, ErrClass: ErrTimeout},
		{Addr: a, Kind: KindScan, Probe: "judge", OK: false, ErrClass: ErrRefused},
	}
	if err := d.RecordMeasurements(ctx, facts); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		p, err := d.GetProxyByAddr(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != StatusAlive || p.OKCount != 2 || p.FailCount != 0 || p.OKStreak != 2 ||
			p.LastMS != 140 || p.BestMS != 120 || p.MSCount != 2 || p.ExitIP != "203.0.113.9" {
			t.Fatalf("%s: %+v", when, p)
		}
		ms, _ := d.Measurements(ctx, p.ID, 0)
		if len(ms) != 5 {
			t.Fatalf("%s: %d facts kept", when, len(ms))
		}
	}
	check("incremental")
	if _, err := d.RebuildStats(ctx); err != nil {
		t.Fatal(err)
	}
	check("rebuilt")
	// A failed connectivity probe still marks it dead.
	if _, err := d.RecordMeasurement(ctx, Measurement{Addr: a, Kind: KindScan, Probe: "cloudflare", OK: false, ErrClass: ErrTimeout}); err != nil {
		t.Fatal(err)
	}
	if p, _ := d.GetProxyByAddr(ctx, a); p.Status != StatusDead || p.FailStreak != 1 {
		t.Fatalf("connectivity failure: %+v", p)
	}
	if !IsAuxProbe("judge") || IsAuxProbe("cloudflare") || NotAuxCond() != "probe NOT IN ('https','judge','exit-ip')" {
		t.Fatal("aux helpers")
	}
}
