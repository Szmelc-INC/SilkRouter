package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHomeFromEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	if got := Home(); got != dir {
		t.Fatalf("Home() = %q, want %q", got, dir)
	}
	if HomeSource() != "$"+EnvHome {
		t.Fatalf("HomeSource() = %q", HomeSource())
	}
}

func TestHomeFromXDG(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv(EnvHome, "")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	want := filepath.Join(xdg, "szmelc", "proxy")
	if got := Home(); got != want {
		t.Fatalf("Home() = %q, want %q", got, want)
	}
}

func TestHomeDefault(t *testing.T) {
	fake := t.TempDir()
	t.Setenv(EnvHome, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", fake)
	t.Setenv("USERPROFILE", fake) // Windows
	want := filepath.Join(fake, ".config", "szmelc", "proxy")
	if got := Home(); got != want {
		t.Fatalf("Home() = %q, want %q", got, want)
	}
	if HomeSource() != "default" {
		t.Fatalf("HomeSource() = %q", HomeSource())
	}
}

func TestLayoutHelpers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)
	cases := map[string]string{
		Config():      filepath.Join(dir, "config.toml"),
		DB():          filepath.Join(dir, "silkrouter.db"),
		ListsDir():    filepath.Join(dir, "lists"),
		LogsDir():     filepath.Join(dir, "logs"),
		ShellDir():    filepath.Join(dir, "shell"),
		RunDir():      filepath.Join(dir, "run"),
		LegacyCache(): filepath.Join(dir, "cache.list"),
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestConfigPathPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)
	t.Setenv(EnvConfig, "")
	if got := ConfigPath(""); got != filepath.Join(dir, "config.toml") {
		t.Fatalf("default ConfigPath = %q", got)
	}
	t.Setenv(EnvConfig, filepath.Join(dir, "env.toml"))
	if got := ConfigPath(""); got != filepath.Join(dir, "env.toml") {
		t.Fatalf("env ConfigPath = %q", got)
	}
	if got := ConfigPath(filepath.Join(dir, "flag.toml")); got != filepath.Join(dir, "flag.toml") {
		t.Fatalf("flag ConfigPath = %q", got)
	}
}

func TestEnsureCreatesPrivateDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	t.Setenv(EnvHome, dir)
	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, d := range Dirs() {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", d)
		}
		if perm := info.Mode().Perm(); os.PathSeparator == '/' && perm != DirPerm {
			t.Errorf("%s perm = %o, want %o", d, perm, DirPerm)
		}
	}
	// Idempotent.
	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyCandidates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv(EnvLegacyList, "")
	got := LegacyCandidates()
	if len(got) != 1 || got[0] != filepath.Join(os.TempDir(), "silkrouter-proxy.list") {
		t.Fatalf("LegacyCandidates() = %v", got)
	}
	t.Setenv(EnvLegacyList, filepath.Join(dir, "x.list"))
	if got := LegacyCandidates(); len(got) != 2 || got[0] != filepath.Join(dir, "x.list") {
		t.Fatalf("LegacyCandidates() with PX_LIST = %v", got)
	}
}

func TestExpandAndPretty(t *testing.T) {
	fake := t.TempDir()
	t.Setenv("HOME", fake)
	t.Setenv("USERPROFILE", fake)
	if got := ExpandUser("~/x"); got != filepath.Join(fake, "x") {
		t.Fatalf("ExpandUser = %q", got)
	}
	if got := Pretty(filepath.Join(fake, "a", "b")); got != "~"+string(filepath.Separator)+filepath.Join("a", "b") {
		t.Fatalf("Pretty = %q", got)
	}
}
