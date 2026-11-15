package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/paths"
)

// TestMain lets the test binary double as a harmless program to route
// (see TestRouteRunsProgram).
func TestMain(m *testing.M) {
	if out := os.Getenv("SILK_HELPER_OUT"); out != "" {
		_ = os.WriteFile(out, []byte(os.Getenv("ALL_PROXY")+"|"+strings.Join(os.Args[1:], " ")), 0o600)
		code := 7
		if v := os.Getenv("SILK_HELPER_EXIT"); v != "" {
			fmt.Sscan(v, &code)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// sandbox points every location SilkRouter touches at temp dirs.
func sandbox(t *testing.T) {
	t.Helper()
	t.Setenv(paths.EnvHome, filepath.Join(t.TempDir(), "home"))
	t.Setenv(paths.EnvConfig, "")
	sandboxTemp(t)
	t.Setenv("PX_LIST", "")
	t.Setenv("NO_COLOR", "1")
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "SILKROUTER_") && k != paths.EnvHome && k != paths.EnvConfig {
			t.Setenv(k, "")
		}
	}
	// Test proxies are fake (closed local ports): measuring them on use
	// would mark them dead. Tests that cover the measurement turn it on.
	t.Setenv("SILKROUTER_HISTORY_MEASURE_ON_USE", "off")
	t.Setenv("SILKROUTER_TTY", "")
}

// sandboxTemp points os.TempDir() at a fresh temp dir on every platform
// (TMPDIR on Unix, TMP/TEMP on Windows), so the first-run import never sees
// a real legacy cache and tests never write into the real temp dir.
func sandboxTemp(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
}

func runApp(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	app := newApp()
	app.Stdout, app.Stderr, app.Stdin = &out, &errb, strings.NewReader("")
	code := app.Run(context.Background(), argv)
	return code, out.String(), errb.String()
}

func TestRegistry(t *testing.T) {
	sandbox(t)
	app := newApp()
	want := []string{"route", "run", "build", "recache", "list", "ls", "check", "blcheck", "proxycheck",
		"probes", "config", "paths", "home", "version", "help", "completion"}
	for _, n := range want {
		if app.Registry.Lookup(n) == nil {
			t.Errorf("command %q not registered", n)
		}
	}
	groups := map[string]bool{}
	for _, g := range cli.DefaultGroupOrder {
		groups[g] = true
	}
	for _, c := range app.Registry.Visible() {
		if c.Summary == "" || !groups[c.Group] {
			t.Errorf("%s: summary %q, group %q", c.Name, c.Summary, c.Group)
		}
		paths := [][]string{{c.Name}}
		for _, s := range c.Subcommands {
			paths = append(paths, []string{c.Name, s.Name})
		}
		for _, p := range paths {
			var b bytes.Buffer
			app.WriteCommandHelp(&b, "px", p, nil)
			if !strings.Contains(b.String(), "px "+strings.Join(p, " ")) {
				t.Errorf("help for %v:\n%s", p, b.String())
			}
		}
	}
}

func TestLegacyResolution(t *testing.T) {
	sandbox(t)
	reg := newApp().Registry
	cases := map[string]string{
		"curl ifconfig.me":      "route",
		"-d 500 -s curl x":      "route",
		"-- build":              "route",
		"--fresh curl x":        "route",
		"-B -j 5":               "build",
		"-R":                    "recache",
		"-L":                    "list",
		"--list":                "list",
		"-F file.txt":           "build",
		"ls":                    "list",
		"run curl":              "route",
		"config set a b":        "config set",
		"home":                  "paths",
		"-x -p socks5://h:1 id": "route",
	}
	for line, want := range cases {
		res, err := reg.Resolve(strings.Fields(line))
		if err != nil || res.PathString() != want {
			t.Errorf("%q -> %q, %v; want %q", line, res.PathString(), err, want)
		}
	}
}

func TestCompletionScripts(t *testing.T) {
	sandbox(t)
	dir := t.TempDir()
	for _, sh := range cli.Shells {
		code, out, errs := runApp(t, "px", "completion", sh)
		if code != 0 {
			t.Fatalf("completion %s: %d %s", sh, code, errs)
		}
		for _, c := range newApp().Registry.Visible() {
			for _, n := range c.Names() {
				if !strings.Contains(out, n) {
					t.Errorf("%s script misses %q", sh, n)
				}
			}
			for _, s := range c.Subcommands {
				if !strings.Contains(out, s.Name) {
					t.Errorf("%s script misses %s %s", sh, c.Name, s.Name)
				}
			}
		}
		for _, f := range []string{"max-ms", "test-url", "builtin", "from-file", "no-color"} {
			if !strings.Contains(out, f) {
				t.Errorf("%s script misses flag %q", sh, f)
			}
		}
		bin, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s not installed; skipping syntax check", sh)
			continue
		}
		f := filepath.Join(dir, "c."+sh)
		os.WriteFile(f, []byte(out), 0o600)
		if msg, err := exec.Command(bin, "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s -n: %v\n%s", sh, err, msg)
		}
	}
	if code, _, _ := runApp(t, "px", "completion", "csh"); code != 2 {
		t.Fatalf("unknown shell exit %d", code)
	}
	if _, err := os.Stat(paths.Home()); err == nil {
		t.Fatal("completion created the config home")
	}
}

func TestDynamicCompletion(t *testing.T) {
	sandbox(t)
	_, out, _ := runApp(t, "px", "__complete", "probes")
	if !strings.Contains(out, "cloudflare\t") {
		t.Fatalf("probes: %q", out)
	}
	_, out, _ = runApp(t, "px", "__complete", "config-keys")
	if !strings.Contains(out, "route.max_ms\t") {
		t.Fatalf("config-keys: %q", out)
	}
	_, out, _ = runApp(t, "px", "__complete", "commands")
	if !strings.Contains(out, "route\t") || strings.Contains(out, "__complete") {
		t.Fatalf("commands: %q", out)
	}
	if code, _, _ := runApp(t, "px", "__complete", "nope"); code != 2 {
		t.Fatalf("unknown topic exit %d", code)
	}
}

func TestConfigCommands(t *testing.T) {
	sandbox(t)
	cfgPath := filepath.Join(paths.Home(), "config.toml")

	_, out, _ := runApp(t, "px", "config", "path")
	if strings.TrimSpace(out) != cfgPath {
		t.Fatalf("config path = %q", out)
	}
	if code, _, errs := runApp(t, "px", "config", "set", "route.max_ms", "500"); code != 0 {
		t.Fatalf("set: %d %s", code, errs)
	}
	if _, out, _ = runApp(t, "px", "config", "get", "route.max_ms"); out != "500\n" {
		t.Fatalf("get = %q", out)
	}
	t.Setenv("SILKROUTER_ROUTE_MAX_MS", "650")
	if _, out, _ = runApp(t, "px", "cfg", "get", "route.max_ms"); out != "650\n" {
		t.Fatalf("env override: get = %q", out)
	}
	t.Setenv("SILKROUTER_ROUTE_MAX_MS", "")

	_, out, _ = runApp(t, "px", "config", "show", "--json")
	var m map[string]map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("show --json: %v\n%s", err, out)
	}
	if m["route"]["max_ms"] != float64(500) || m["build"]["timeout"] != "15s" {
		t.Fatalf("show --json: %v", m["route"])
	}
	_, out, _ = runApp(t, "px", "config", "show", "--sources")
	if !strings.Contains(out, "max_ms = 500  # file") {
		t.Fatalf("show --sources:\n%s", out)
	}

	if code, _, errs := runApp(t, "px", "config", "set", "route.verify", "sometimes"); code != 2 || !strings.Contains(errs, "auto, skip, always") {
		t.Fatalf("invalid set: %d %s", code, errs)
	}
	if code, _, errs := runApp(t, "px", "config", "get", "route.maxms"); code != 2 || !strings.Contains(errs, "route.max_ms") {
		t.Fatalf("unknown key: %d %s", code, errs)
	}

	if code, _, _ := runApp(t, "px", "config", "reset", "route.max_ms"); code != 0 {
		t.Fatal("reset key failed")
	}
	if _, out, _ = runApp(t, "px", "config", "get", "route.max_ms"); out != "1000\n" {
		t.Fatalf("after reset: %q", out)
	}

	if code, _, _ := runApp(t, "px", "config", "init"); code != 1 {
		t.Fatalf("init over an existing file should fail, got %d", code)
	}
	if code, _, _ := runApp(t, "px", "config", "init", "--force"); code != 0 {
		t.Fatal("init --force failed")
	}
	if _, err := os.Stat(cfgPath + ".bak"); err != nil {
		t.Fatalf("no backup: %v", err)
	}

	// A broken file is reported, and set refuses to overwrite it.
	os.WriteFile(cfgPath, []byte("[route\n"), 0o600)
	code, _, errs := runApp(t, "px", "config", "set", "route.max_ms", "1")
	if code != 1 || !strings.Contains(errs, "config init --force") {
		t.Fatalf("set on a broken file: %d %s", code, errs)
	}
	if data, _ := os.ReadFile(cfgPath); string(data) != "[route\n" {
		t.Fatal("broken file was overwritten")
	}

	_, out, _ = runApp(t, "px", "config", "keys", "--json")
	if !strings.Contains(out, `"name": "route.max_ms"`) {
		t.Fatalf("keys --json: %s", out)
	}
}

func TestPathsCommand(t *testing.T) {
	sandbox(t)
	_, out, _ := runApp(t, "px", "paths", "--json")
	var rows []paths.Entry
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 8 {
		t.Fatalf("paths --json: %v %s", err, out)
	}
	if _, out, _ = runApp(t, "px", "home", "cache"); strings.TrimSpace(out) != paths.LegacyCache() {
		t.Fatalf("home cache = %q", out)
	}
	if code, _, _ := runApp(t, "px", "paths", "nope"); code != 2 {
		t.Fatal("unknown path name accepted")
	}
	if _, err := os.Stat(paths.Home()); err == nil {
		t.Fatal("paths created the config home")
	}
}

func TestVersionAndArgv0(t *testing.T) {
	sandbox(t)
	var outs []string
	for _, argv0 := range []string{"px", "/usr/bin/s-proxy", `C:\bin\silkrouter.exe`} {
		code, out, _ := runApp(t, argv0, "version")
		if code != 0 {
			t.Fatalf("%s version: %d", argv0, code)
		}
		outs = append(outs, out)
		if _, out2, _ := runApp(t, argv0, "--version"); out2 != out {
			t.Fatalf("--version %q != version %q", out2, out)
		}
	}
	if outs[0] != "silkrouter "+version+"\n" || outs[1] != outs[0] || outs[2] != outs[0] {
		t.Fatalf("version outputs differ: %q", outs)
	}
}

func TestFirstRunImport(t *testing.T) {
	sandbox(t)
	legacy := filepath.Join(os.TempDir(), "silkrouter-proxy.list")
	os.WriteFile(legacy, []byte("socks5://1.2.3.4:1080 [120ms]\nhttp://5.6.7.8:3128 [80ms]\nnot a proxy\n"), 0o600)

	code, out, errs := runApp(t, "px", "-L")
	if code != 0 || !strings.Contains(errs, "imported legacy cache") || !strings.Contains(out, "total: [2]") {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errs)
	}
	data, err := os.ReadFile(paths.LegacyCache())
	if err != nil || !strings.HasPrefix(string(data), "http://5.6.7.8:3128 [80ms]") {
		t.Fatalf("imported cache: %q %v", data, err)
	}
	// Second run: nothing to import.
	if _, _, errs = runApp(t, "px", "list"); strings.Contains(errs, "imported") {
		t.Fatalf("imported twice: %s", errs)
	}

	// $PX_LIST stays the active cache.
	pxList := filepath.Join(t.TempDir(), "mine.list")
	os.WriteFile(pxList, []byte("http://9.9.9.9:80 [5ms]\n"), 0o600)
	t.Setenv("PX_LIST", pxList)
	if _, out, _ = runApp(t, "px", "list", "--json"); !strings.Contains(out, "9.9.9.9") {
		t.Fatalf("PX_LIST ignored: %s", out)
	}
}

func TestRouteRunsProgram(t *testing.T) {
	sandbox(t)
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	outFile := filepath.Join(t.TempDir(), "helper.out")
	t.Setenv("SILK_HELPER_OUT", outFile)

	check := func(want string, argv ...string) {
		t.Helper()
		os.Remove(outFile)
		code, _, errs := runApp(t, argv...)
		if code != 7 {
			t.Fatalf("%q: exit %d (want the program's 7)\n%s", argv, code, errs)
		}
		data, _ := os.ReadFile(outFile)
		if string(data) != want {
			t.Fatalf("%q: program saw %q, want %q", argv, data, want)
		}
	}
	// Legacy style: px.sh options, then the program and its own flags.
	check("http://127.0.0.1:9|-x --help", "px", "-q", "-x", "-p", "http://127.0.0.1:9", self, "-x", "--help")
	// Explicit route and "--".
	check("socks5://127.0.0.1:9|a", "s-proxy", "route", "-q", "-x", "-p", "socks5://127.0.0.1:9", "--", self, "a")
	check("http://127.0.0.1:9|", "silkrouter", "-q", "-x", "-p", "http://127.0.0.1:9", "--", self)
}

func TestUnknownProgram(t *testing.T) {
	sandbox(t)
	code, _, errs := runApp(t, "px", "biuld")
	if code != 127 || !strings.Contains(errs, "did you mean 'px build'") {
		t.Fatalf("typo: %d %s", code, errs)
	}
	code, _, errs = runApp(t, "px", "--", "no-such-program-xyz")
	if code != 127 || !strings.Contains(errs, "command not found") {
		t.Fatalf("missing program: %d %s", code, errs)
	}
}

func TestRecacheAcceptsBuildFlags(t *testing.T) {
	sandbox(t)
	// Source flags are hidden for recache but still parse (exit 1 = no cache,
	// not 2 = usage error).
	code, _, errs := runApp(t, "px", "recache", "-O", "-builtin", "-j", "3")
	if code != 1 || !strings.Contains(errs, "no cache to re-test") {
		t.Fatalf("recache: %d %s", code, errs)
	}
	_, out, _ := runApp(t, "px", "recache", "--help")
	if strings.Contains(out, "from-file") {
		t.Fatal("hidden flag shown in recache help")
	}
}

func TestHelpOutput(t *testing.T) {
	sandbox(t)
	code, out, _ := runApp(t, "px", "help")
	if code != 0 || !strings.Contains(out, "px [global options] <command>") || !strings.Contains(out, "CONFIGURATION") {
		t.Fatalf("help: %d\n%s", code, out)
	}
	code, out, _ = runApp(t, "px", "help", "cfg", "set")
	if code != 0 || !strings.Contains(out, "px config set <key> <value>") {
		t.Fatalf("help config set: %d\n%s", code, out)
	}
	if code, _, _ = runApp(t, "px", "help", "nope"); code != 2 {
		t.Fatalf("help nope: %d", code)
	}
	code, out, _ = runApp(t, "px", "build", "--help")
	for _, want := range []string{"-j, --workers, --jobs N", "default: 200", "config: build.workers", "--probe NAME"} {
		if !strings.Contains(out, want) {
			t.Errorf("build help misses %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatalf("build --help exit %d", code)
	}
}
