package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

// ---- harness ------------------------------------------------------------------

type harness struct {
	t       *testing.T
	m       *Model
	d       *db.DB
	cfgPath string
	clip    *bytes.Buffer
	quit    bool
	ids     map[string]int64
}

const (
	fastURL  = "socks5://192.0.2.1:1080"
	slowURL  = "http://192.0.2.2:8080"
	dirtyURL = "http://192.0.2.3:3128"
	deadURL  = "http://192.0.2.4:80"
	newURL   = "socks4://192.0.2.5:1080"
)

// sandbox isolates the config home and clears SILKROUTER_* overrides.
func sandbox(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "SILKROUTER_") {
			t.Setenv(k, "")
		}
	}
	t.Setenv("SILKROUTER_HOME", home)
	return home
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

func measure(t *testing.T, d *db.DB, id int64, ok bool, ms, n int) {
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
		if _, err := d.RecordUsage(context.Background(), db.Usage{ProxyID: id, ProxyURL: url, Argv: []string{cmd, "https://example.com"},
			ExitCode: &code, TSEnd: time.Now(), SessionID: "sess-1"}); err != nil {
			t.Fatal(err)
		}
	}
}

func seed(t *testing.T, d *db.DB) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	ids := map[string]int64{
		"fast": addProxy(t, d, fastURL), "slow": addProxy(t, d, slowURL), "dirty": addProxy(t, d, dirtyURL),
		"dead": addProxy(t, d, deadURL), "new": addProxy(t, d, newURL),
	}
	measure(t, d, ids["fast"], true, 40, 12)
	measure(t, d, ids["slow"], true, 900, 12)
	measure(t, d, ids["dirty"], true, 60, 12)
	measure(t, d, ids["dead"], false, 0, 3)
	if err := d.UpsertReputation(ctx,
		db.Reputation{IP: "192.0.2.3", ListName: "tor-exits", ListKind: "torexit", Listed: true},
		db.Reputation{IP: "192.0.2.1", ListName: "zen.spamhaus.org", ListKind: "dnsbl", Listed: false},
	); err != nil {
		t.Fatal(err)
	}
	use(t, d, ids["fast"], fastURL, "curl", 0, 11)
	use(t, d, ids["slow"], slowURL, "git", 1, 4)
	if err := d.SetTag(ctx, db.Tag{ProxyID: ids["fast"], Tag: "pinned", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := score.RefreshTags(ctx, d, score.TagOptions{}); err != nil {
		t.Fatal(err)
	}
	run, err := d.CreateScanRun(ctx, db.ScanRun{Mode: "full", Total: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateScanRun(ctx, run, 10, "5", db.ScanDone, 0); err != nil {
		t.Fatal(err)
	}
	return ids
}

func newHarness(t *testing.T, mut ...func(*Options)) *harness {
	t.Helper()
	home := sandbox(t)
	d, err := db.Open(filepath.Join(home, "silkrouter.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ids := seed(t, d)
	cfgPath := filepath.Join(home, "config.toml")
	cfg, err := config.Load(cfgPath, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	clip := &bytes.Buffer{}
	env := map[string]string{"SILKROUTER_ACTIVE": "1", "SILKROUTER_PROXY": fastURL, "SILKROUTER_SESSION": "sess-1"}
	opts := Options{DB: d, Config: cfg, ConfigPath: cfgPath, Version: "4.0.0-test", Color: "never", ShowDead: true,
		Clipboard: clip, Getenv: func(k string) string { return env[k] }, Output: io.Discard}
	for _, f := range mut {
		f(&opts)
	}
	m, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	m.tick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil } // no timers in tests
	h := &harness{t: t, m: m, d: d, cfgPath: cfgPath, clip: clip, ids: ids}
	h.send(tea.WindowSizeMsg{Width: 150, Height: 40})
	h.run(m.Init())
	return h
}

// run executes cmd synchronously, feeding every message it produces back
// through Update until nothing is left.
func (h *harness) run(cmd tea.Cmd) {
	h.t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 2000 {
			h.t.Fatal("runaway command loop")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tea.QuitMsg:
			h.quit = true
		default:
			_, next := h.m.Update(msg)
			queue = append(queue, next)
		}
	}
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	_, cmd := h.m.Update(msg)
	h.run(cmd)
}

var namedKeys = map[string]tea.KeyType{
	"enter": tea.KeyEnter, "esc": tea.KeyEsc, "tab": tea.KeyTab, "shift+tab": tea.KeyShiftTab, "up": tea.KeyUp,
	"down": tea.KeyDown, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
	"backspace": tea.KeyBackspace, "ctrl+c": tea.KeyCtrlC, "ctrl+u": tea.KeyCtrlU, "ctrl+r": tea.KeyCtrlR,
}

func (h *harness) key(ks ...string) {
	h.t.Helper()
	for _, k := range ks {
		if t, ok := namedKeys[k]; ok {
			h.send(tea.KeyMsg{Type: t})
			continue
		}
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	}
}

func (h *harness) typ(s string) {
	h.t.Helper()
	for _, r := range s {
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func (h *harness) palette(line string) {
	h.t.Helper()
	h.key(":")
	h.typ(line)
	h.key("enter")
}

func (h *harness) view() string { return ansi.Strip(h.m.View()) }

func (h *harness) mustSee(parts ...string) {
	h.t.Helper()
	v := h.view()
	for _, p := range parts {
		if !strings.Contains(v, p) {
			h.t.Fatalf("view lacks %q:\n%s", p, v)
		}
	}
}

func (h *harness) mustNotSee(parts ...string) {
	h.t.Helper()
	v := h.view()
	for _, p := range parts {
		if strings.Contains(v, p) {
			h.t.Fatalf("view shows %q:\n%s", p, v)
		}
	}
}

func (h *harness) tags(name string) string {
	h.t.Helper()
	ts, err := h.d.Tags(context.Background(), h.ids[name])
	if err != nil {
		h.t.Fatal(err)
	}
	var ls []string
	for _, t := range ts {
		ls = append(ls, t.Label())
	}
	return " " + strings.Join(ls, " ") + " "
}

// selectProxy narrows the proxy table to one proxy with the live search.
func (h *harness) selectProxy(search string) {
	h.t.Helper()
	h.key("2", "/")
	h.key("ctrl+u")
	h.typ(search)
	h.key("enter")
}

// ---- tests ------------------------------------------------------------------------

func TestDashboard(t *testing.T) {
	h := newHarness(t)
	h.mustSee("Dashboard", "5 proxies in the database", "alive", "Alive latency", "Shell session", "proxy on", fastURL,
		"sess-1", "15 commands logged", "Recent scans", "full", "done", "Top commands", "curl", "Tags", "v4.0.0-test")
	if h.m.active != 0 {
		t.Fatalf("start tab %d", h.m.active)
	}
}

func TestStartOptions(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Tab = "prox"; o.Status = "alive"; o.Sort = "last_ms"; o.Search = "192.0.2" })
	if h.m.activeView() != view(h.m.prox) {
		t.Fatal("--tab prefix not honoured")
	}
	h.mustSee("status:alive", "sort:last_ms ↑", "/192.0.2", fastURL)
	h.mustNotSee(deadURL)
	for _, bad := range []func(*Options){
		func(o *Options) { o.Tab = "nope" }, func(o *Options) { o.Theme = "nope" },
		func(o *Options) { o.Sort = "nope" }, func(o *Options) { o.Status = "nope" },
	} {
		o := Options{DB: h.d, Output: io.Discard}
		bad(&o)
		if _, err := New(context.Background(), o); err == nil {
			t.Fatalf("bad option accepted: %+v", o)
		}
	}
}

func TestProxiesSearchSortFilter(t *testing.T) {
	h := newHarness(t)
	h.key("2")
	h.mustSee("PROXY", "STATUS", "RANK", fastURL, slowURL, dirtyURL, deadURL, newURL, "1-5 of 5", "sort:rank ↑")

	h.key("/")
	h.typ("192.0.2.2")
	h.mustSee(slowURL, "1-1 of 1", "/192.0.2.2")
	h.mustNotSee(fastURL)
	h.key("esc") // cancel: the previous (empty) search comes back
	h.mustSee(fastURL, "1-5 of 5")

	h.key("a") // alive
	h.mustSee("status:alive")
	h.mustNotSee(deadURL, newURL)
	h.key("s")
	h.mustSee("sort:last_ms ↑")
	h.key("r")
	h.mustSee("sort:last_ms ↓")
	if f := h.m.prox.f.dbFilter(); f.Sort != "last_ms" || !f.Reverse || len(f.Status) != 1 {
		t.Fatalf("filter %+v", f)
	}
	h.key("p") // http
	h.mustSee("scheme:http", slowURL)
	h.mustNotSee(fastURL)
	h.key("X")
	h.mustSee(fastURL, deadURL, "filters cleared")

	// Dead rows hidden while the status filter is all.
	h.key("D")
	h.mustSee("(dead hidden)")
	h.mustNotSee(deadURL)
	h.key("D")
	h.mustSee(deadURL)

	// Cursor movement and the status of the selection.
	h.key("end")
	if h.m.prox.tbl.cursor != 4 {
		t.Fatalf("cursor %d", h.m.prox.tbl.cursor)
	}
	h.key("home", "j")
	if h.m.prox.tbl.cursor != 1 {
		t.Fatalf("cursor %d", h.m.prox.tbl.cursor)
	}
}

func TestDetailPane(t *testing.T) {
	h := newHarness(t)
	h.selectProxy("192.0.2.1")
	h.key("enter")
	h.mustSee(fastURL, "alive", "Recent measurements", "(12 shown", "scan 12/12", "pinned", "latency", "40ms", "proxy #")
	h.key("end") // the bottom of the pane
	h.mustSee("Reputation", "zen.spamhaus.org", "Usage", "(11 commands)", "curl https://example.com")
	h.key("esc")
	if h.m.prox.detail != nil {
		t.Fatal("esc did not close the details")
	}
	h.mustSee("1-1 of 1")
}

func TestTagBlockCopy(t *testing.T) {
	h := newHarness(t)
	h.selectProxy("192.0.2.1")

	h.key("t")
	h.mustSee("add tag(s) to " + fastURL)
	h.typ("work team:curl")
	h.key("enter")
	if tg := h.tags("fast"); !strings.Contains(tg, " work ") || !strings.Contains(tg, " team:curl ") {
		t.Fatalf("tags after t: %s", tg)
	}
	h.mustSee("tagged " + fastURL)

	h.key("T")
	h.typ("work")
	h.key("enter")
	if tg := h.tags("fast"); strings.Contains(tg, " work ") || !strings.Contains(tg, " pinned ") {
		t.Fatalf("tags after T: %s", tg)
	}

	h.key("b")
	if !strings.Contains(h.tags("fast"), " blocked ") {
		t.Fatalf("b did not block: %s", h.tags("fast"))
	}
	h.mustSee("blocked " + fastURL)
	h.key("b")
	if strings.Contains(h.tags("fast"), " blocked ") {
		t.Fatal("second b did not unblock")
	}

	// The same from the details pane and from the palette.
	h.key("enter")
	h.palette("block")
	if !strings.Contains(h.tags("fast"), " blocked ") {
		t.Fatal(":block")
	}
	h.palette("unblock")
	h.palette("tag add vip")
	if tg := h.tags("fast"); strings.Contains(tg, " blocked ") || !strings.Contains(tg, " vip ") {
		t.Fatalf("palette tags: %s", tg)
	}
	h.palette("tag rm vip")
	if strings.Contains(h.tags("fast"), " vip ") {
		t.Fatal(":tag rm")
	}

	h.key("y")
	want := base64.StdEncoding.EncodeToString([]byte(fastURL))
	if out := h.clip.String(); !strings.Contains(out, "]52;c;"+want) {
		t.Fatalf("OSC 52 sequence: %q", out)
	}
	h.mustSee("copied " + fastURL)
}

func TestCheckRecordsMeasurement(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // a closed local port: the check fails without any real network
	h := newHarness(t, func(o *Options) {
		o.Config.Check.Timeout = config.Seconds(3)
		o.Config.Check.Tries = 1
	})
	id := addProxy(t, h.d, "http://"+addr)
	h.selectProxy("127.0.0.1")
	h.mustSee("http://" + addr)
	h.key("c")
	ms, err := h.d.Measurements(context.Background(), id, 10)
	if err != nil || len(ms) != 1 {
		t.Fatalf("measurements %v %v", ms, err)
	}
	if m := ms[0]; m.OK || m.Kind != db.KindProbe || m.Probe != "cloudflare" || m.ErrClass == "" {
		t.Fatalf("measurement %+v", m)
	}
	p, _ := h.d.GetProxy(context.Background(), id)
	if p.Status != db.StatusDead || p.FailCount != 1 {
		t.Fatalf("proxy after check: %+v", p)
	}
	h.mustSee("failed", "dead")
	if len(h.m.jobs) != 0 {
		t.Fatalf("job not finished: %v", h.m.jobs)
	}
}

func TestPalette(t *testing.T) {
	h := newHarness(t)
	// Completion: "ta" + tab -> tab, " hi" + tab -> history.
	h.key(":")
	h.typ("ta")
	h.mustSee("tab", "tag")
	h.key("tab")
	if v := h.m.pal.input.Value(); v != "tab" {
		t.Fatalf("completed %q", v)
	}
	h.typ(" hi")
	h.key("tab")
	if v := h.m.pal.input.Value(); v != "tab history " {
		t.Fatalf("completed %q", v)
	}
	h.key("enter")
	if h.m.activeView() != view(h.m.hist) {
		t.Fatal(":tab history")
	}

	h.palette("sort ok_streak desc")
	if f := h.m.prox.f; f.sort != "ok_streak" || f.reverse || h.m.activeView() != view(h.m.prox) {
		t.Fatalf(":sort: %+v", f)
	}
	h.palette("sort last_ms desc")
	if !h.m.prox.f.reverse {
		t.Fatal(":sort last_ms desc")
	}
	h.palette("filter status=alive scheme=socks tag=fast max_ms=500")
	f := h.m.prox.f
	if f.status != "alive" || f.scheme != "socks" || f.tag != "fast" || f.maxMS != 500 {
		t.Fatalf(":filter: %+v", f)
	}
	h.mustSee(fastURL, "tag:fast", "≤500ms")
	h.mustNotSee(slowURL)

	out := filepath.Join(t.TempDir(), "out.txt")
	h.palette("export " + out)
	data, err := os.ReadFile(out)
	if err != nil || strings.TrimSpace(string(data)) != fastURL {
		t.Fatalf("export: %q %v", data, err)
	}
	h.mustSee("exported 1 proxy URLs")

	h.palette("filter")
	if h.m.prox.f.tag != "" || h.m.prox.f.status != "all" {
		t.Fatal(":filter (clear)")
	}
	h.palette("search 192.0.2.3")
	h.mustSee(dirtyURL, "1-1 of 1")

	h.palette("theme neon")
	if h.m.theme != "neon" {
		t.Fatal(":theme")
	}
	h.palette("nope")
	h.mustSee("unknown command :nope")
	h.palette("tab 4")
	if h.m.activeView() != view(h.m.tagv) {
		t.Fatal(":tab 4")
	}
	h.palette("help")
	if !h.m.help.open {
		t.Fatal(":help")
	}
	h.key("esc")
	h.palette("q")
	if !h.quit {
		t.Fatal(":q did not quit")
	}
}

func TestPaletteShowAndSet(t *testing.T) {
	h := newHarness(t)
	h.palette("show 192.0.2.2:8080")
	if h.m.prox.detail == nil || h.m.prox.detail.id != h.ids["slow"] {
		t.Fatal(":show")
	}
	h.mustSee(slowURL, "Recent measurements")
	h.palette("set route.max_ms 700")
	c, err := config.LoadFile(h.cfgPath)
	if err != nil || c.Route.MaxMS != 700 {
		t.Fatalf(":set: %v %v", c.Route.MaxMS, err)
	}
	h.palette("set route.max_ms lots")
	h.mustSee("invalid integer")
}

func TestConfigEdit(t *testing.T) {
	h := newHarness(t)
	h.key("7")
	h.mustSee("KEY", "ORIGIN", "route.max_ms", "default")
	h.key("/")
	h.typ("route.max_ms")
	h.key("enter")
	h.mustSee("maximum cached latency")

	h.key("enter") // edit
	h.key("ctrl+u")
	h.typ("abc")
	h.mustSee("invalid integer")
	h.key("enter")
	if _, err := os.Stat(h.cfgPath); err == nil {
		t.Fatal("an invalid value was written")
	}
	h.key("ctrl+u")
	h.typ("500")
	h.key("enter")
	c, err := config.LoadFile(h.cfgPath)
	if err != nil || c.Route.MaxMS != 500 {
		t.Fatalf("config.toml after edit: %v %v", c.Route.MaxMS, err)
	}
	if h.m.cfg.Route.MaxMS != 500 || h.m.cfg.Origin("route.max_ms") != config.FromFile {
		t.Fatal("effective config not reloaded")
	}
	h.mustSee("route.max_ms = 500", "file")
	data, _ := os.ReadFile(h.cfgPath)
	if !strings.Contains(string(data), "max_ms = 500") || !strings.Contains(string(data), "# SilkRouter configuration") {
		t.Fatalf("not the canonical file:\n%s", data)
	}

	// x resets (the previous file is kept as .bak).
	h.key("x")
	c, _ = config.LoadFile(h.cfgPath)
	if c.Route.MaxMS != 1000 {
		t.Fatalf("reset: %d", c.Route.MaxMS)
	}
	if _, err := os.Stat(h.cfgPath + ".bak"); err != nil {
		t.Fatal("no backup")
	}

	// [tui] keys apply at once.
	h.key("/", "ctrl+u")
	h.typ("tui.theme")
	h.key("enter", "enter", "ctrl+u")
	h.typ("mono")
	h.key("enter")
	if h.m.theme != "mono" {
		t.Fatalf("theme %q after editing tui.theme", h.m.theme)
	}
	h.key("/", "ctrl+u")
	h.typ("tui.columns")
	h.key("enter", "enter", "ctrl+u")
	h.typ("id,proxy,cc")
	h.key("enter")
	if len(h.m.prox.cols) != 3 {
		t.Fatalf("columns %d", len(h.m.prox.cols))
	}
	h.key("2")
	h.mustSee("CC")
	h.mustNotSee("STREAK")

	// Unknown columns are reported, the known ones still shown.
	h.palette("set tui.columns id,proxy,bogus")
	h.mustSee("tui.columns: unknown bogus")
	if len(h.m.prox.cols) != 2 {
		t.Fatalf("columns %d", len(h.m.prox.cols))
	}
}

func TestHistoryAndStats(t *testing.T) {
	h := newHarness(t)
	h.key("3")
	h.mustSee("TIME", "COMMAND", "curl https://example.com", "git https://example.com", "1-15 of 15")
	h.key("f") // ok only
	h.mustSee("result:ok", "1-11 of 11")
	h.mustNotSee("git https")
	h.key("f", "f", "f") // back to all
	h.key("g")
	h.mustSee("USES", "groups by proxy", fastURL, slowURL)
	h.key("g")
	h.mustSee("groups by cmd", "CMD")
	h.key("enter") // curl (most used) -> the log narrowed to it
	h.mustSee("cmd:curl", "1-11 of 11")
	h.mustNotSee("git https")
	h.key("X")
	h.mustSee("1-15 of 15")

	// Enter on a log row opens its proxy.
	h.key("home", "enter")
	if h.m.activeView() != view(h.m.prox) || h.m.prox.detail == nil {
		t.Fatal("enter on a usage row did not open its proxy")
	}
	h.mustSee("Recent measurements")
}

func TestTagsView(t *testing.T) {
	h := newHarness(t)
	h.key("4")
	h.mustSee("TAG", "PROXIES", "pinned", "manual", "stable", "heuristic")
	h.key("/")
	h.typ("pinn")
	h.key("enter")
	h.mustSee("manual: your label")
	h.key("enter")
	if h.m.activeView() != view(h.m.prox) || h.m.prox.f.tag != "pinned" {
		t.Fatal("enter did not show the tagged proxies")
	}
	h.mustSee(fastURL, "tag:pinned", "1-1 of 1")

	h.key("4", "R")
	h.mustSee("set ")
	h.mustSee("heuristic tags")
}

func TestScansAndRank(t *testing.T) {
	h := newHarness(t)
	h.key("5")
	h.mustSee("MODE", "STATE", "full", "done", "10/10", "rank order: balanced")
	if p, _ := h.d.GetProxy(context.Background(), h.ids["fast"]); p.Rank != nil {
		t.Fatal("seeded proxy already ranked")
	}
	h.key("k")
	p, _ := h.d.GetProxy(context.Background(), h.ids["fast"])
	if p.Rank == nil || p.SpeedScore == nil {
		t.Fatalf("k did not rank: %+v", p)
	}
	h.mustSee("ranked 3 alive (balanced)")
	h.palette("rank speed")
	h.mustSee("(speed)")
	h.palette("rank fastest")
	h.mustSee("unknown rank mode")
}

func TestListsView(t *testing.T) {
	h := newHarness(t)
	h.key("6")
	h.mustSee("LIST", "ENTRIES", "tor-exits", "never", "16 lists")
}

func TestHelpOverlay(t *testing.T) {
	for _, md := range []bool{true, false} {
		h := newHarness(t, func(o *Options) { o.Config.TUI.Markdown = md })
		h.key("2", "?")
		if !h.m.help.open {
			t.Fatal("? did not open the help")
		}
		h.mustSee("Proxies keys", "live search")
		content := ansi.Strip(h.m.help.content)
		for _, want := range []string{"Moving around", "Everywhere", "Command palette", ":filter", ":export FILE", ":theme NAME", "Mouse"} {
			if !strings.Contains(content, want) {
				t.Fatalf("markdown=%v: help lacks %q:\n%s", md, want, content)
			}
		}
		h.key("pgdown", "esc")
		if h.m.help.open {
			t.Fatal("esc did not close the help")
		}
		h.key("?", "?")
		if h.m.help.open {
			t.Fatal("? did not toggle the help")
		}
	}
}

func TestQuitConfirm(t *testing.T) {
	h := newHarness(t)
	id, _ := h.m.startJob("check #1")
	h.key("q")
	if !h.m.confirming || h.quit {
		t.Fatal("q quit while a job runs")
	}
	h.mustSee("check #1 running: quit anyway?")
	h.key("n")
	if h.m.confirming || h.quit {
		t.Fatal("n did not cancel")
	}
	h.key("q", "y")
	if !h.quit {
		t.Fatal("y did not quit")
	}

	h2 := newHarness(t, func(o *Options) { o.Config.TUI.ConfirmQuit = false })
	h2.m.startJob("rank")
	h2.key("q")
	if !h2.quit {
		t.Fatal("confirm_quit=false still asked")
	}
	_ = id
}

func TestColorsAndThemes(t *testing.T) {
	h := newHarness(t)
	if strings.Contains(h.m.View(), "\x1b[") {
		t.Fatal("escape sequences with Color=never")
	}
	hc := newHarness(t, func(o *Options) { o.Color = "always" })
	hc.key("2")
	if !strings.Contains(hc.m.View(), "\x1b[") {
		t.Fatal("no colors with Color=always")
	}
	for _, th := range ThemeNames() {
		if err := hc.m.setTheme(th); err != nil {
			t.Fatal(err)
		}
		for i := range hc.m.views {
			hc.m.active = i
			if v := hc.m.View(); v == "" {
				t.Fatalf("%s/%d: empty view", th, i)
			}
		}
	}
}

func TestLayoutFitsTheTerminal(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Color = "always" })
	for _, size := range [][2]int{{150, 40}, {80, 24}, {40, 10}} {
		h.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for i := range h.m.views {
			h.key(string(rune('1' + i)))
			for _, overlay := range []string{"", "?"} {
				if overlay != "" {
					h.key(overlay)
				}
				lines := strings.Split(h.m.View(), "\n")
				if len(lines) != size[1] {
					t.Fatalf("%dx%d tab %d help=%v: %d lines", size[0], size[1], i+1, overlay != "", len(lines))
				}
				for _, l := range lines {
					if w := lipgloss.Width(l); w > size[0] {
						t.Fatalf("%dx%d tab %d: line of %d cells: %q", size[0], size[1], i+1, w, ansi.Strip(l))
					}
				}
				if overlay != "" {
					h.key("esc")
				}
			}
		}
	}
}

func TestMouse(t *testing.T) {
	h := newHarness(t)
	h.key("2")
	h.send(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if h.m.prox.tbl.cursor != 3 {
		t.Fatalf("wheel: cursor %d", h.m.prox.tbl.cursor)
	}
	// Body line 0 is the header, so screen row 3 is table row 1.
	h.send(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 5, Y: 3})
	if h.m.prox.tbl.cursor != 1 {
		t.Fatalf("click: cursor %d", h.m.prox.tbl.cursor)
	}
	h.send(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 5, Y: 3})
	if h.m.prox.detail == nil {
		t.Fatal("second click did not open the row")
	}
	_, _, spans := h.m.tabSpans()
	h.send(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: spans[2][0] + 1, Y: 0})
	if h.m.activeView() != view(h.m.hist) {
		t.Fatal("click on the History tab")
	}
}

func TestAutoRefreshTick(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Refresh = time.Second })
	h.key("2")
	addProxy(t, h.d, "http://198.51.100.7:3128")
	h.send(tickMsg{seq: h.m.tickSeq})
	h.mustSee("198.51.100.7")
	h.send(tickMsg{seq: h.m.tickSeq - 1}) // stale ticks are ignored
}

// TestRunProgram drives the real Bubble Tea loop end to end with scripted
// input (no terminal): switch to the proxy table, open the palette, quit.
func TestRunProgram(t *testing.T) {
	h := newHarness(t)
	out := &syncBuffer{}
	in, feed := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{DB: h.d, Config: h.m.cfg, ConfigPath: h.cfgPath, Color: "never",
			Input: in, Output: out})
	}()
	for _, chunk := range []string{"2", ":sort last_ms\r", "q"} {
		time.Sleep(300 * time.Millisecond) // let a frame or two render
		if _, err := feed.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
	}
	feed.Close()
	screen := ansi.Strip(out.String())
	for _, want := range []string{"PROXIES", fastURL, "sort:last_ms"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("the program never drew %q:\n%s", want, screen)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the renderer goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Runes that arrive in one read come as one KeyMsg; they must act as
// separate keys (a paste stays text).
func TestCoalescedKeys(t *testing.T) {
	h := newHarness(t)
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2/192.0.2.3")})
	if h.m.activeView() != view(h.m.prox) || h.m.prox.f.search != "192.0.2.3" {
		t.Fatalf("tab %d search %q", h.m.active, h.m.prox.f.search)
	}
	h.mustSee(dirtyURL, "1-1 of 1")
	h.key("ctrl+u")
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q?:x"), Paste: true})
	if h.quit || h.m.prox.f.search != "q?:x" {
		t.Fatalf("paste acted as keys: quit=%v search=%q", h.quit, h.m.prox.f.search)
	}
	h.key("enter")
	// History: switching to the stats and pressing enter before the
	// groups arrive must not touch the old rows.
	h.key("3")
	_, cmd := h.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	h.send(tea.KeyMsg{Type: tea.KeyEnter})
	h.run(cmd)
	h.mustSee("groups by proxy")
}

func TestTabsCycle(t *testing.T) {
	h := newHarness(t)
	h.key("tab", "tab")
	if h.m.active != 2 {
		t.Fatalf("tab tab -> %d", h.m.active)
	}
	h.key("shift+tab", "shift+tab", "shift+tab")
	if h.m.active != len(h.m.views)-1 {
		t.Fatalf("wrap -> %d", h.m.active)
	}
	h.key("ctrl+r")
	h.mustSee("reloaded config")
}
