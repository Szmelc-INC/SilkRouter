package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/source"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustRun(t *testing.T, argv ...string) string {
	t.Helper()
	code, out, errs := runApp(t, argv...)
	if code != 0 {
		t.Fatalf("%q: exit %d\n%s\n%s", argv, code, out, errs)
	}
	return out
}

func TestDBReadOnlyCommandsCreateNothing(t *testing.T) {
	sandbox(t)
	if out := mustRun(t, "px", "db", "path"); strings.TrimSpace(out) != paths.DB() {
		t.Fatalf("db path: %q", out)
	}
	out := mustRun(t, "px", "db", "info")
	if !strings.Contains(out, "not created yet") {
		t.Fatalf("db info: %s", out)
	}
	if _, err := os.Stat(paths.Home()); !os.IsNotExist(err) {
		t.Fatal("read-only db commands created the config home")
	}
	if code, _, _ := runApp(t, "px", "db", "migrate", "--status"); code != 1 {
		t.Fatal("migrate --status on a missing db")
	}
	// db.path is honoured everywhere.
	alt := filepath.Join(t.TempDir(), "alt.db")
	t.Setenv("SILKROUTER_DB_PATH", alt)
	if out := mustRun(t, "px", "db", "path"); strings.TrimSpace(out) != alt {
		t.Fatalf("db.path: %q", out)
	}
	if out := mustRun(t, "px", "paths", "db"); strings.TrimSpace(out) != alt {
		t.Fatalf("paths db: %q", out)
	}
}

func TestAutoImportBuiltin(t *testing.T) {
	sandbox(t)
	old := source.Builtin
	source.Builtin = []byte("socks5://203.0.113.1:1080\nhttp://203.0.113.2:8080\nhttp://203.0.113.2:8080\njunk\n")
	t.Cleanup(func() { source.Builtin = old })

	code, out, errs := runApp(t, "px", "proxies", "--json")
	if code != 0 || !strings.Contains(errs, "imported 2 proxies (embedded list)") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	var rows []db.Proxy
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 2 || rows[0].Source != "builtin" {
		t.Fatalf("%v %s", err, out)
	}
	// Only once; and `list` (alive only) keeps its legacy meaning.
	code, out, errs = runApp(t, "px", "list")
	if code != 0 || strings.Contains(errs, "imported") || !strings.Contains(out, "total: [0]") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	// db.auto_import = false leaves a fresh database empty.
	t.Setenv("SILKROUTER_DB_PATH", filepath.Join(t.TempDir(), "x.db"))
	t.Setenv("SILKROUTER_DB_AUTO_IMPORT", "false")
	if out := mustRun(t, "px", "proxies", "--count"); strings.TrimSpace(out) != "0" {
		t.Fatalf("auto_import=false: %s", out)
	}
}

func TestDBCommands(t *testing.T) {
	sandbox(t)
	cacheFile := writeFile(t, "old.list", "socks5://198.51.100.1:1080 [120ms]\nhttp://198.51.100.2:3128 [80ms]\nsocks4://198.51.100.3:1080\n")
	out := mustRun(t, "px", "db", "import", cacheFile, "--cache", "--json")
	var res db.ImportResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Added != 3 || res.Measurements != 3 {
		t.Fatalf("%v %s", err, out)
	}
	plain := writeFile(t, "plain.txt", "http://198.51.100.2:3128\nhttps://198.51.100.4:443\n")
	if out = mustRun(t, "px", "db", "import", plain, "--source", "mine"); !strings.Contains(out, "1 new, 1 known") {
		t.Fatalf("import: %s", out)
	}
	if code, _, _ := runApp(t, "px", "db", "import"); code != 2 {
		t.Fatal("import without args")
	}

	// proxies: table, filters, count, urls, json.
	out = mustRun(t, "px", "proxies")
	if !strings.Contains(out, "PROXY") || !strings.Contains(out, "http://198.51.100.2:3128") || !strings.Contains(out, "alive") {
		t.Fatalf("table:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 || !strings.Contains(lines[1], "198.51.100.2") {
		t.Fatalf("order (fastest first):\n%s", out)
	}
	if out = mustRun(t, "px", "proxies", "--status", "alive", "--socks", "-u"); out != "socks5://198.51.100.1:1080\nsocks4://198.51.100.3:1080\n" {
		t.Fatalf("socks alive urls: %q", out)
	}
	if out = mustRun(t, "px", "proxies", "-d", "100", "--count"); strings.TrimSpace(out) != "1" {
		t.Fatalf("count: %q", out)
	}
	if out = mustRun(t, "px", "proxies", "--source", "mine", "-u"); out != "https://198.51.100.4:443\n" {
		t.Fatalf("source: %q", out)
	}
	if out = mustRun(t, "px", "db-ls", "198.51.100.3", "-u"); out != "socks4://198.51.100.3:1080\n" {
		t.Fatalf("search: %q", out)
	}
	if code, _, errs := runApp(t, "px", "proxies", "--sort", "nope"); code != 2 || !strings.Contains(errs, "rank") {
		t.Fatalf("bad sort: %d %s", code, errs)
	}
	if code, _, _ := runApp(t, "px", "proxies", "--status", "zombie"); code != 2 {
		t.Fatal("bad status accepted")
	}
	code, _, errs := runApp(t, "px", "proxies", "-n", "2")
	if code != 0 || !strings.Contains(errs, "showing 1-2 of 4") {
		t.Fatalf("paging: %s", errs)
	}

	// show by URL / id, text and JSON.
	out = mustRun(t, "px", "show", "socks5://198.51.100.1:1080")
	if !strings.Contains(out, "Recent measurements") || !strings.Contains(out, "legacy") || !strings.Contains(out, "120ms") {
		t.Fatalf("show:\n%s", out)
	}
	out = mustRun(t, "px", "show", "1", "--json")
	var sh showOutput
	if err := json.Unmarshal([]byte(out), &sh); err != nil || sh.Proxy.ID != 1 || len(sh.Measurements) != 1 {
		t.Fatalf("%v %s", err, out)
	}
	if code, _, _ := runApp(t, "px", "show", "203.0.113.200"); code != 1 {
		t.Fatal("show unknown")
	}

	// export: legacy list round trip, csv, file output.
	out = mustRun(t, "px", "db", "export", "--status", "alive")
	if out != "http://198.51.100.2:3128 [80ms]\nsocks5://198.51.100.1:1080 [120ms]\nsocks4://198.51.100.3:1080\n" {
		t.Fatalf("export list:\n%s", out)
	}
	dst := filepath.Join(t.TempDir(), "out.csv")
	mustRun(t, "px", "db", "export", "--format", "csv", "-o", dst)
	if data, _ := os.ReadFile(dst); !strings.HasPrefix(string(data), "id,url,") || strings.Count(string(data), "\n") != 5 {
		t.Fatalf("csv: %s", data)
	}

	// The exported list is a valid legacy cache.
	exported := writeFile(t, "exp.list", mustRun(t, "px", "db", "export"))
	if out = mustRun(t, "px", "list", "-f", exported); !strings.Contains(out, "total: [4]") {
		t.Fatalf("list -f export:\n%s", out)
	}
	// list in database mode: alive proxies only.
	if out = mustRun(t, "px", "list"); !strings.Contains(out, "total: [3]") || !strings.Contains(out, "database") {
		t.Fatalf("list db:\n%s", out)
	}

	// info, rebuild-stats, migrate, vacuum.
	out = mustRun(t, "px", "db", "info", "--json")
	var info dbInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil || info.Stats.Proxies != 4 || info.Stats.ByStatus["alive"] != 3 {
		t.Fatalf("%v %s", err, out)
	}
	if out = mustRun(t, "px", "db", "info"); !strings.Contains(out, "alive") || !strings.Contains(out, "schema") {
		t.Fatalf("info:\n%s", out)
	}
	if out = mustRun(t, "px", "db", "rebuild-stats"); !strings.Contains(out, "4 proxies") && !strings.Contains(out, "3 proxies") {
		t.Fatalf("rebuild: %s", out)
	}
	if out = mustRun(t, "px", "db", "migrate"); !strings.Contains(out, "applied") {
		t.Fatalf("migrate: %s", out)
	}
	mustRun(t, "px", "db", "vacuum")

	// reset: refuses without --yes on a non-terminal, backs up with it.
	if code, _, errs := runApp(t, "px", "db", "reset"); code != 1 || !strings.Contains(errs, "--yes") {
		t.Fatalf("reset without --yes: %d %s", code, errs)
	}
	code, _, errs = runApp(t, "px", "db", "reset", "--yes", "--empty")
	if code != 0 || !strings.Contains(errs, "backup:") {
		t.Fatalf("reset: %d %s", code, errs)
	}
	baks, _ := filepath.Glob(paths.DB() + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("backups: %v", baks)
	}
	if out = mustRun(t, "px", "proxies", "--count"); strings.TrimSpace(out) != "0" {
		t.Fatalf("after reset: %s", out)
	}
	bak, err := db.Open(baks[0])
	if err != nil {
		t.Fatal(err)
	}
	defer bak.Close()
	if n, _ := bak.CountProxies(context.Background(), db.Filter{}); n != 4 {
		t.Fatalf("backup holds %d proxies", n)
	}
}

func TestRouteFromDatabase(t *testing.T) {
	sandbox(t)
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	outFile := filepath.Join(t.TempDir(), "helper.out")
	t.Setenv("SILK_HELPER_OUT", outFile)
	cacheFile := writeFile(t, "c.list", "socks5://127.0.0.1:9 [50ms]\nhttp://127.0.0.2:9 [900ms]\n")
	mustRun(t, "px", "db", "import", cacheFile, "--cache")

	run := func(argv ...string) (int, string, string) {
		os.Remove(outFile)
		code, _, errs := runApp(t, argv...)
		data, _ := os.ReadFile(outFile)
		return code, string(data), errs
	}
	if code, got, errs := run("px", "-q", "-x", "-s", self, "a"); code != 7 || got != "socks5://127.0.0.1:9|a" {
		t.Fatalf("socks pick: %d %q %s", code, got, errs)
	}
	if code, got, errs := run("px", "-q", "-x", "-D", "100", self); code != 7 || got != "http://127.0.0.2:9|" {
		t.Fatalf("min-ms pick: %d %q %s", code, got, errs)
	}
	if code, _, errs := run("px", "-q", "-x", "-H", "-d", "100", self); code != 1 || !strings.Contains(errs, "no proxies match (database") {
		t.Fatalf("no match: %d %s", code, errs)
	}
	// -f keeps the legacy text-cache behaviour.
	legacy := writeFile(t, "l.list", "socks5://127.0.0.3:9 [10ms]\n")
	if code, got, errs := run("px", "-q", "-x", "-f", legacy, self); code != 7 || got != "socks5://127.0.0.3:9|" {
		t.Fatalf("-f: %d %q %s", code, got, errs)
	}
	// route.pool = cache picks from <home>/cache.list.
	if err := os.WriteFile(paths.LegacyCache(), []byte("http://127.0.0.4:9 [20ms]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SILKROUTER_ROUTE_POOL", "cache")
	if code, got, errs := run("px", "-q", "-x", self); code != 7 || got != "http://127.0.0.4:9|" {
		t.Fatalf("pool=cache: %d %q %s", code, got, errs)
	}
}

func TestBuildRecordsIntoDatabase(t *testing.T) {
	sandbox(t)
	// Proxies on closed local ports: every probe fails fast, no network.
	list := writeFile(t, "cands.txt", "socks5://127.0.0.1:9\nhttp://127.0.0.1:9\n")
	code, _, errs := runApp(t, "px", "-q", "build", "-F", list, "-j", "2", "-n", "1", "-w", "2s", "--test-url", "http://127.0.0.1:9/")
	if code != 1 || !strings.Contains(errs, "0 working") {
		t.Fatalf("build: %d %s", code, errs)
	}
	if _, err := os.Stat(paths.LegacyCache()); !os.IsNotExist(err) {
		t.Fatal("database-mode build wrote the text cache")
	}
	out := mustRun(t, "px", "proxies", "--json")
	var rows []db.Proxy
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("%v %s", err, out)
	}
	for _, p := range rows {
		if p.Status != db.StatusDead || p.FailCount != 1 || p.Source != "file:cands.txt" {
			t.Fatalf("%+v", p)
		}
	}
	out = mustRun(t, "px", "show", "socks5://127.0.0.1:9", "--json")
	var sh showOutput
	json.Unmarshal([]byte(out), &sh)
	if len(sh.Measurements) != 1 || sh.Measurements[0].ErrClass == "" || sh.Measurements[0].Kind != db.KindProbe {
		t.Fatalf("%s", out)
	}
	// recache in database mode with nothing alive.
	if code, _, errs = runApp(t, "px", "recache"); code != 1 || !strings.Contains(errs, "no cache to re-test") {
		t.Fatalf("recache: %d %s", code, errs)
	}
}
