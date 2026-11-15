package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/paths"
)

// localProxy is a fake HTTP proxy that answers every request itself (204 for
// generate_204 probes), so route measurements and quick scans succeed
// without any network.
func localProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				if req.Method == http.MethodConnect {
					fmt.Fprint(c, "HTTP/1.1 200 OK\r\n\r\n")
					if req, err = http.ReadRequest(br); err != nil {
						return
					}
				}
				status := 200
				if strings.HasSuffix(req.URL.Path, "generate_204") {
					status = 204
				}
				fmt.Fprintf(c, "HTTP/1.1 %d X\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status)
			}(c)
		}
	}()
	return "http://" + ln.Addr().String()
}

func TestUsageHistoryStatsAndTags(t *testing.T) {
	sandbox(t)
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("SILK_HELPER_OUT", filepath.Join(t.TempDir(), "helper.out"))
	t.Setenv("SILK_HELPER_EXIT", "0")
	t.Setenv("SILKROUTER_HISTORY_MEASURE_ON_USE", "parallel")
	t.Setenv("SILKROUTER_TAGS_FAV_MIN_USES", "3")
	t.Setenv("SILKROUTER_TAGS_FAV_MIN_RATE", "0.7") // 3 of the 4 uses below succeed
	t.Setenv("SILKROUTER_SESSION", "sess1")
	px := localProxy(t)
	mustRun(t, "px", "db", "import", writeFile(t, "c.list", px+" [20ms]\n"), "--cache")
	cmd := strings.TrimSuffix(filepath.Base(self), ".exe")

	for i := 0; i < 3; i++ {
		if code, _, errs := runApp(t, "px", "-q", "-x", self, "arg", fmt.Sprint(i)); code != 0 {
			t.Fatalf("route %d: %d %s", i, code, errs)
		}
	}
	t.Setenv("SILK_HELPER_EXIT", "28")
	if code, _, _ := runApp(t, "px", "-q", "-x", self, "fail"); code != 28 {
		t.Fatalf("exit code not passed through: %d", code)
	}

	// history: newest first, everything recorded.
	var rows []db.Usage
	if err := json.Unmarshal([]byte(mustRun(t, "px", "history", "--json")), &rows); err != nil || len(rows) != 4 {
		t.Fatalf("history: %v %d", err, len(rows))
	}
	u := rows[0]
	if u.Mode != db.ModeRoute || u.Cmd != cmd || u.ExitCode == nil || *u.ExitCode != 28 || u.ErrClass != db.ErrExitNonzero ||
		u.SessionID != "sess1" || u.ProxyID != 1 || u.ProxyURL != px || u.Cwd == "" || u.PID == 0 {
		t.Fatalf("usage row: %+v", u)
	}
	if rows[1].MS <= 0 || rows[1].ExitCode == nil || *rows[1].ExitCode != 0 {
		t.Fatalf("latency measured on use: %+v", rows[1])
	}
	if out := mustRun(t, "px", "history", "--status", "fail", "--json"); strings.Count(out, `"id"`) != 1 {
		t.Fatalf("--status fail: %s", out)
	}
	if out := mustRun(t, "px", "history"); !strings.Contains(out, "COMMAND") || !strings.Contains(out, "arg 2") {
		t.Fatalf("history table:\n%s", out)
	}
	if out := mustRun(t, "px", "log", "-c", cmd, "-s", "1h", "--full"); !strings.Contains(out, "session=sess1") {
		t.Fatalf("history --full:\n%s", out)
	}
	if data, err := os.ReadFile(filepath.Join(paths.LogsDir(), "usage.log")); err != nil || strings.Count(string(data), "\n") != 4 {
		t.Fatalf("text log: %v %q", err, data)
	}

	// The proxy's counters and recent delay.
	var sh showOutput
	json.Unmarshal([]byte(mustRun(t, "px", "show", "1", "--json")), &sh)
	p := sh.Proxy
	if p.UseCount != 4 || p.UseOK != 3 || p.UseFail != 1 || p.LastCmd != cmd || p.LastUsedAt.IsZero() || sh.UsageCount != 4 {
		t.Fatalf("counters: %+v", p)
	}
	routeFacts := 0
	for _, m := range sh.Measurements {
		if m.Kind == db.KindRoute && m.OK {
			routeFacts++
		}
	}
	if routeFacts != 4 || p.Status != db.StatusAlive {
		t.Fatalf("route measurements: %d %+v", routeFacts, sh.Measurements)
	}

	// stats per proxy, per proxy+cmd, per error.
	var groups []db.UsageGroup
	json.Unmarshal([]byte(mustRun(t, "px", "stats", "--json")), &groups)
	if len(groups) != 1 || groups[0].Uses != 4 || groups[0].OK != 3 || groups[0].Fail != 1 || groups[0].AvgMS <= 0 {
		t.Fatalf("stats: %+v", groups)
	}
	json.Unmarshal([]byte(mustRun(t, "px", "history", "stats", "--by", "proxy,cmd", "--json")), &groups)
	if len(groups) != 1 || groups[0].Key[1] != cmd {
		t.Fatalf("stats by proxy,cmd: %+v", groups)
	}
	json.Unmarshal([]byte(mustRun(t, "px", "stats", "--by", "err", "--sort", "key", "--json")), &groups)
	if len(groups) != 2 || groups[0].Key[0] != "exit_nonzero" || groups[1].Key[0] != "ok" {
		t.Fatalf("stats by err: %+v", groups)
	}
	if out := mustRun(t, "px", "stats", "--by", "day"); !strings.Contains(out, "USES") {
		t.Fatalf("stats table:\n%s", out)
	}
	if code, _, _ := runApp(t, "px", "stats", "--by", "nope"); code != 2 {
		t.Fatal("bad --by accepted")
	}

	// Tags: fav:<cmd> after 3 good uses, recommended:<cmd>; favourites / recommend.
	if out := mustRun(t, "px", "tags"); !strings.Contains(out, "fav:"+cmd) || !strings.Contains(out, "recommended:"+cmd) {
		t.Fatalf("tags:\n%s", out)
	}
	if out := mustRun(t, "px", "favorites", cmd, "-u"); strings.TrimSpace(out) != px {
		t.Fatalf("favorites: %q", out)
	}
	if out := mustRun(t, "px", "recommend", "--pick", cmd); strings.TrimSpace(out) != px {
		t.Fatalf("recommend: %q", out)
	}
	// A manual "blocked" tag keeps the proxy out of route; rm brings it back.
	mustRun(t, "px", "tags", "add", "1", "blocked")
	if code, _, errs := runApp(t, "px", "-q", "-x", self); code != 1 || !strings.Contains(errs, "no proxies match") {
		t.Fatalf("blocked proxy used: %d %s", code, errs)
	}
	mustRun(t, "px", "tags", "rm", "1", "blocked")
	t.Setenv("SILK_HELPER_EXIT", "0")
	if code, _, errs := runApp(t, "px", "-q", "-x", "--tag", "fav", self); code != 0 {
		t.Fatalf("--tag fav: %d %s", code, errs)
	}
	if code, _, errs := runApp(t, "px", "-q", "-x", "--tag", "nope", self); code != 1 || !strings.Contains(errs, "tag=nope") {
		t.Fatalf("--tag nope: %d %s", code, errs)
	}

	// Shell hook logging.
	now := time.Now().UnixMilli()
	mustRun(t, "px", "__shell-log", "--proxy", px, "--session", "sess2", "--start", fmt.Sprint(now-1500),
		"--end", fmt.Sprint(now), "--exit", "7", "--cwd", "/tmp", "--", "curl", "-s", "https://example.com")
	mustRun(t, "px", "__shell-log", "--proxy", px, "--exit", "0", "--", "ls", "-la") // skipped command
	json.Unmarshal([]byte(mustRun(t, "px", "history", "--mode", "shell", "--json")), &rows)
	if len(rows) != 1 || rows[0].Cmd != "curl" || rows[0].ErrClass != db.ErrRefused || rows[0].DurationMS != 1500 || rows[0].ProxyID != 1 {
		t.Fatalf("shell log: %+v", rows)
	}

	// export / prune.
	if out := mustRun(t, "px", "history", "export"); !strings.HasPrefix(out, "id,ts_start") || strings.Count(out, "\n") != 7 {
		t.Fatalf("export:\n%s", out)
	}
	if code, _, _ := runApp(t, "px", "history", "prune", "--older-than", "1d"); code != 0 {
		t.Fatal("prune with nothing to prune")
	}

	// quick scan through the fake proxy, scan runs, rank.
	out := mustRun(t, "px", "scan", "quick", "--json", "--probes", "cloudflare")
	var sum struct {
		State string `json:"state"`
		Done  int    `json:"done"`
		Alive int    `json:"alive"`
	}
	if err := json.Unmarshal([]byte(out), &sum); err != nil || sum.State != db.ScanDone || sum.Done != 1 || sum.Alive != 1 {
		t.Fatalf("scan quick: %v %s", err, out)
	}
	if out := mustRun(t, "px", "scan", "runs"); !strings.Contains(out, "quick") || !strings.Contains(out, "done") {
		t.Fatalf("scan runs:\n%s", out)
	}
	var rk struct {
		Best []*db.Proxy `json:"best"`
	}
	json.Unmarshal([]byte(mustRun(t, "px", "rank", "--json")), &rk)
	if len(rk.Best) != 1 || rk.Best[0].Rank == nil || *rk.Best[0].Rank != 1 || rk.Best[0].SpeedScore == nil {
		t.Fatalf("rank: %+v", rk.Best)
	}
	if code, _, errs := runApp(t, "px", "-q", "-x", "--best", self); code != 0 {
		t.Fatalf("--best: %d %s", code, errs)
	}
	if out := mustRun(t, "px", "proxies", "--sort", "use_count", "--json"); !strings.Contains(out, `"use_count": 7`) {
		t.Fatalf("use_count sort:\n%s", out)
	}
}

func TestListsCommands(t *testing.T) {
	sandbox(t)
	t.Setenv("SILKROUTER_SCAN_LIST_NAMES", "tor-exits,firehol-proxies")
	out := mustRun(t, "px", "lists")
	if !strings.Contains(out, "tor-exits") || !strings.Contains(out, "never") || strings.Contains(out, "x4b-vpn") {
		t.Fatalf("lists:\n%s", out)
	}
	if _, err := os.Stat(paths.ListsDir()); !os.IsNotExist(err) {
		t.Fatal("'px lists' created the lists dir")
	}
	// Offline check against a local copy (no download).
	dir := paths.ListsDir()
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "tor-exits.txt"), []byte("185.220.101.1\n"), 0o600)
	out = mustRun(t, "px", "lists", "check", "185.220.101.1", "--dnsbl=false")
	if !strings.Contains(out, "listed on 1") || !strings.Contains(out, "tor-exits") {
		t.Fatalf("check:\n%s", out)
	}
	if out = mustRun(t, "px", "lists", "check", "socks5://192.0.2.1:1080", "--dnsbl=false"); !strings.Contains(out, "clean") {
		t.Fatalf("check clean:\n%s", out)
	}
	t.Setenv("SILKROUTER_SCAN_LIST_NAMES", "nope")
	if code, _, _ := runApp(t, "px", "lists"); code != 2 {
		t.Fatal("unknown list accepted")
	}
}
