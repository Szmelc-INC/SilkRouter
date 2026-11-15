package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/paths"
)

// envMap returns a lookupEnv backed by m (tests never read the real env).
func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsAreValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	if c.Route.MaxMS != 1000 || c.Build.Workers != 200 || c.Build.Timeout.D() != 15*time.Second {
		t.Fatalf("unexpected defaults: %+v", c.Route)
	}
	for _, k := range Keys() {
		if k.Help == "" {
			t.Errorf("key %s has no help text", k.Name)
		}
		if k.Env[0] != EnvName(k.Name) {
			t.Errorf("key %s: first env %q", k.Name, k.Env[0])
		}
	}
}

func TestDeclaredSections(t *testing.T) {
	have := map[string]bool{}
	for _, tb := range Tables() {
		have[tb.Name] = true
	}
	for _, want := range []string{"cache", "route", "build", "ui", "db", "scan", "tags", "history", "shell", "tui"} {
		if !have[want] {
			t.Errorf("table [%s] not declared", want)
		}
	}
}

func TestPrecedence(t *testing.T) {
	path := writeFile(t, `
[route]
max_ms = 500
min_ms = 20
[cache]
file = "/from/file.list"
`)
	// File beats defaults.
	c, err := Load(path, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Route.MaxMS != 500 || c.Route.MinMS != 20 || c.Origin("route.max_ms") != FromFile {
		t.Fatalf("file layer: max=%d min=%d origin=%s", c.Route.MaxMS, c.Route.MinMS, c.Origin("route.max_ms"))
	}
	if c.Origin("route.scheme") != FromDefault {
		t.Fatalf("untouched key origin = %s", c.Origin("route.scheme"))
	}

	// Environment beats the file; SILKROUTER_* beats legacy names.
	c, err = Load(path, envMap(map[string]string{
		"SILKROUTER_ROUTE_MAX_MS": "700",
		"PX_LIST":                 "/legacy.list",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Route.MaxMS != 700 || c.Origin("route.max_ms") != "env SILKROUTER_ROUTE_MAX_MS" {
		t.Fatalf("env layer: max=%d origin=%s", c.Route.MaxMS, c.Origin("route.max_ms"))
	}
	if c.Cache.File != "/legacy.list" || c.Origin("cache.file") != "env PX_LIST" {
		t.Fatalf("PX_LIST: file=%q origin=%s", c.Cache.File, c.Origin("cache.file"))
	}
	c, _ = Load(path, envMap(map[string]string{
		"SILKROUTER_CACHE_FILE": "/new.list",
		"PX_LIST":               "/legacy.list",
	}))
	if c.Cache.File != "/new.list" {
		t.Fatalf("SILKROUTER_CACHE_FILE should win over PX_LIST, got %q", c.Cache.File)
	}

	// Empty and invalid environment values are ignored.
	c, _ = Load(path, envMap(map[string]string{
		"SILKROUTER_ROUTE_MAX_MS": "",
		"SILKROUTER_ROUTE_MIN_MS": "lots",
	}))
	if c.Route.MaxMS != 500 || c.Route.MinMS != 20 {
		t.Fatalf("bad env should be ignored: max=%d min=%d", c.Route.MaxMS, c.Route.MinMS)
	}
	if len(c.Warnings()) != 1 || !strings.Contains(c.Warnings()[0], "SILKROUTER_ROUTE_MIN_MS") {
		t.Fatalf("warnings = %v", c.Warnings())
	}

	// LoadFile never applies the environment.
	t.Setenv("SILKROUTER_ROUTE_MAX_MS", "900")
	c, _ = LoadFile(path)
	if c.Route.MaxMS != 500 {
		t.Fatalf("LoadFile applied env: %d", c.Route.MaxMS)
	}
}

func TestMissingFileIsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"), envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.FileExists() || c.Route.MaxMS != 1000 {
		t.Fatalf("exists=%v max=%d", c.FileExists(), c.Route.MaxMS)
	}
}

func TestUnknownKeyAndInvalidValue(t *testing.T) {
	path := writeFile(t, `
[route]
max_ms = 250
verify = "sometimes"
bogus = 1
[build]
workers = 0
`)
	c, err := Load(path, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Route.MaxMS != 250 {
		t.Fatalf("valid key lost: %d", c.Route.MaxMS)
	}
	if c.Route.Verify != "auto" || c.Build.Workers != 200 {
		t.Fatalf("invalid values should fall back to defaults: verify=%q workers=%d", c.Route.Verify, c.Build.Workers)
	}
	joined := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{`route.bogus`, `route.verify`, `build.workers`} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings miss %s:\n%s", want, joined)
		}
	}
	if c.Origin("route.verify") != FromDefault {
		t.Fatalf("reverted key origin = %s", c.Origin("route.verify"))
	}
}

func TestParseError(t *testing.T) {
	path := writeFile(t, "[route\nmax_ms = ")
	c, err := Load(path, envMap(map[string]string{"SILKROUTER_ROUTE_MAX_MS": "42"}))
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if c == nil || c.Route.MaxMS != 42 {
		t.Fatalf("a broken file should still yield defaults + env, got %+v", c)
	}
}

func TestGetSetRoundTrip(t *testing.T) {
	c := Default()
	for _, k := range Keys() {
		v, err := c.Get(k.Name)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Set(k.Name, v); err != nil {
			t.Errorf("Set(%s, %q): %v", k.Name, v, err)
		}
		if got, _ := c.Get(k.Name); got != v {
			t.Errorf("%s round trip: %q -> %q", k.Name, v, got)
		}
		if !c.IsDefault(k.Name) {
			t.Errorf("%s no longer default after round trip", k.Name)
		}
	}

	sets := map[string]string{
		"route.max_ms":     "500",
		"route.scheme":     "socks",
		"route.verify":     "always",
		"build.timeout":    "1m30s",
		"check.timeout":    "2.5",
		"source.builtin":   "yes",
		"cache.file":       "~/x.list",
		"blcheck.token":    `we"ird`,
		"ui.color":         "never",
		"route.auto_build": "true",
	}
	for k, v := range sets {
		if err := c.Set(k, v); err != nil {
			t.Fatalf("Set(%s, %q): %v", k, v, err)
		}
	}
	if c.Build.Timeout.D() != 90*time.Second || c.Check.Timeout.D() != 2500*time.Millisecond || !c.Source.Builtin {
		t.Fatalf("typed values: %+v %+v", c.Build, c.Check)
	}

	// Save + LoadFile reproduces every value; untouched keys stay defaults.
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '/' && info.Mode().Perm() != 0o600 {
		t.Errorf("config perm = %o", info.Mode().Perm())
	}
	back, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Warnings()) != 0 {
		t.Fatalf("warnings after round trip: %v", back.Warnings())
	}
	for _, k := range Keys() {
		want, _ := c.Get(k.Name)
		got, _ := back.Get(k.Name)
		if got != want {
			t.Errorf("%s: saved %q, loaded %q", k.Name, want, got)
		}
	}
	if back.Origin("route.max_ms") != FromFile || back.Origin("route.min_ms") != FromDefault {
		t.Fatalf("commented defaults should not count as set: %s %s", back.Origin("route.max_ms"), back.Origin("route.min_ms"))
	}
	if got := back.CacheFile(); got != paths.ExpandUser("~/x.list") {
		t.Fatalf("CacheFile = %q", got)
	}
}

func TestSetValidation(t *testing.T) {
	c := Default()
	bad := map[string]string{
		"route.verify":  "maybe",
		"build.workers": "0",
		"route.max_ms":  "fast",
		"build.timeout": "soon",
		"check.timeout": "-1",
		"ui.color":      "rainbow",
		"route.bogus":   "1",
	}
	for k, v := range bad {
		if err := c.Set(k, v); err == nil {
			t.Errorf("Set(%s, %q) accepted", k, v)
		}
	}
	if _, err := c.Get("nope.key"); err == nil {
		t.Fatal("Get of unknown key succeeded")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("failed sets changed the config: %v", err)
	}
}

func TestResetKey(t *testing.T) {
	c := Default()
	_ = c.Set("route.max_ms", "5")
	if err := c.Reset("route.max_ms"); err != nil {
		t.Fatal(err)
	}
	if c.Route.MaxMS != 1000 {
		t.Fatalf("reset: %d", c.Route.MaxMS)
	}
}

func TestDurationForms(t *testing.T) {
	path := writeFile(t, `
[build]
timeout = 7
[check]
timeout = 0.5
[route]
verify_timeout = "1m"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Build.Timeout.D() != 7*time.Second || c.Check.Timeout.D() != 500*time.Millisecond || c.Route.VerifyTimeout.D() != time.Minute {
		t.Fatalf("durations: %v %v %v", c.Build.Timeout, c.Check.Timeout, c.Route.VerifyTimeout)
	}
	for in, want := range map[string]time.Duration{"8": 8 * time.Second, "0.25": 250 * time.Millisecond, "500ms": 500 * time.Millisecond} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "-3", "-1s", "abc"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) accepted", in)
		}
	}
}

func TestWriteDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteDefault(path, false); err != nil {
		t.Fatal(err)
	}
	if err := WriteDefault(path, false); err == nil {
		t.Fatal("WriteDefault overwrote an existing file")
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"[route]", "# max_ms = 1000", "[tags]", "SILKROUTER_ROUTE_MAX_MS", "PX_LIST"} {
		if !strings.Contains(text, want) {
			t.Errorf("default config misses %q", want)
		}
	}
	c, err := LoadFile(path)
	if err != nil || len(c.Warnings()) != 0 {
		t.Fatalf("default file does not load cleanly: %v %v", err, c.Warnings())
	}
	for _, k := range Keys() {
		if c.Origin(k.Name) != FromDefault {
			t.Errorf("%s set by the default file", k.Name)
		}
	}

	bak, err := Backup(path)
	if err != nil || bak != path+".bak" {
		t.Fatalf("Backup = %q, %v", bak, err)
	}
	if bak, err := Backup(filepath.Join(t.TempDir(), "none")); err != nil || bak != "" {
		t.Fatalf("Backup of a missing file = %q, %v", bak, err)
	}
}

func TestWriteSources(t *testing.T) {
	c, _ := Load("", envMap(map[string]string{"SILKROUTER_UI_COLOR": "never"}))
	var b bytes.Buffer
	if err := Write(&b, c, WriteOptions{Sources: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `color = "never"  # env SILKROUTER_UI_COLOR`) {
		t.Fatalf("missing source annotation:\n%s", b.String())
	}
}

func TestCacheFileDefault(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	if got := Default().CacheFile(); got != paths.LegacyCache() {
		t.Fatalf("CacheFile = %q, want %q", got, paths.LegacyCache())
	}
}

func TestMapNested(t *testing.T) {
	m := Default().Map()
	route, ok := m["route"].(map[string]any)
	if !ok || route["max_ms"] != 1000 {
		t.Fatalf("Map()[route] = %#v", m["route"])
	}
	if _, ok := m["tags"]; !ok {
		t.Fatal("empty tables should still appear in Map()")
	}
}
