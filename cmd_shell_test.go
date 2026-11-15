package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/shell"
)

func TestShellOnOffRotate(t *testing.T) {
	sandbox(t)
	for _, k := range []string{shell.EnvActive, shell.EnvSession, shell.EnvShellFn, "ALL_PROXY", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	mustRun(t, "px", "db", "import", writeFile(t, "c.list", "http://127.0.0.1:9 [30ms]\nsocks5://127.0.0.2:9 [40ms]\n"), "--cache")

	// Without the wrapper: explain, fail.
	code, _, errs := runApp(t, "px", "on")
	if code != 1 || !strings.Contains(errs, "shell-init") {
		t.Fatalf("on without wrapper: %d %s", code, errs)
	}
	// Through the wrapper (--emit): exports and a session file.
	code, out, errs := runApp(t, "px", "on", "--emit", "zsh", "-x", "-s")
	if code != 0 || !strings.Contains(out, "export ALL_PROXY='socks5://127.0.0.2:9'") || !strings.Contains(out, "export SILKROUTER_ACTIVE='1'") {
		t.Fatalf("on: %d\n%s\n%s", code, out, errs)
	}
	id := between(out, "export SILKROUTER_SESSION='", "'")
	if id == "" || !strings.Contains(errs, "session "+id) {
		t.Fatalf("session id: %q %s", id, errs)
	}
	if _, err := os.Stat(filepath.Join(paths.RunDir(), "sessions", id+".json")); err != nil {
		t.Fatal(err)
	}
	// Simulate the shell having evaluated it.
	t.Setenv(shell.EnvActive, "1")
	t.Setenv(shell.EnvSession, id)
	t.Setenv("ALL_PROXY", "socks5://127.0.0.2:9")

	// rotate: another proxy, same session; filters reused (-s would leave none,
	// so pass -H explicitly).
	code, out, errs = runApp(t, "px", "rotate", "--emit", "bash", "-x", "-H")
	if code != 0 || !strings.Contains(out, "export ALL_PROXY='http://127.0.0.1:9'") || !strings.Contains(out, "SILKROUTER_SESSION='"+id+"'") {
		t.Fatalf("rotate: %d\n%s\n%s", code, out, errs)
	}
	if strings.Contains(out, "_SILKROUTER_SAVED_") {
		t.Fatal("rotate saved the proxy variables again")
	}
	var sv sessionView
	json.Unmarshal([]byte(mustRun(t, "px", "session", "--json")), &sv)
	if sv.ID != id || !sv.Active || sv.Rotations != 1 || sv.Key != "http://127.0.0.1:9" {
		t.Fatalf("session: %+v", sv)
	}
	if out := mustRun(t, "px", "status"); !strings.Contains(out, "session "+id) || !strings.Contains(out, "active") {
		t.Fatalf("status:\n%s", out)
	}
	if out := mustRun(t, "px", "session", "list"); !strings.Contains(out, id) {
		t.Fatalf("list:\n%s", out)
	}
	// prompt reads the environment only.
	t.Setenv(shell.EnvPrompt, "@ http 127.0.0.1:9 30ms")
	t.Setenv(shell.EnvProxyKey, "http://127.0.0.1:9")
	t.Setenv(shell.EnvProxyMS, "30")
	if out := mustRun(t, "px", "prompt"); strings.TrimSpace(out) != "@ http 127.0.0.1:9 30ms" {
		t.Fatalf("prompt: %q", out)
	}
	if out := mustRun(t, "px", "prompt", "-f", "{scheme}|{host}|{port}|{ms}"); strings.TrimSpace(out) != "http|127.0.0.1|9|30ms" {
		t.Fatalf("prompt -f: %q", out)
	}
	// off: unset, session ended.
	code, out, _ = runApp(t, "px", "off", "--emit", "fish")
	if code != 0 || !strings.Contains(out, "set -e ALL_PROXY") || !strings.Contains(out, "set -e SILKROUTER_SESSION") {
		t.Fatalf("off: %d\n%s", code, out)
	}
	json.Unmarshal([]byte(mustRun(t, "px", "session", "show", id, "--json")), &sv)
	if sv.Active || sv.Ended.IsZero() {
		t.Fatalf("ended: %+v", sv)
	}
	t.Setenv(shell.EnvActive, "")
	os.Unsetenv(shell.EnvActive)
	if code, _, _ := runApp(t, "px", "session"); code != 1 {
		t.Fatal("status without a session")
	}
	// -p with verification against a closed port fails cleanly.
	if code, _, errs := runApp(t, "px", "on", "--emit", "zsh", "-p", "http://127.0.0.1:9", "-t", "1s"); code != 1 || !strings.Contains(errs, "does not answer") {
		t.Fatalf("on -p dead: %d %s", code, errs)
	}
}

func TestShellInitAndInstall(t *testing.T) {
	sandbox(t)
	out := mustRun(t, "px", "shell-init", "zsh")
	if !strings.Contains(out, "__silkrouter_wrap") || !strings.Contains(out, "prompt_silkrouter") || !strings.Contains(out, "__shell-log") {
		t.Fatalf("zsh init:\n%s", out)
	}
	if out := mustRun(t, "px", "init", "bash", "--no-hooks"); strings.Contains(out, "__shell-log") {
		t.Fatal("--no-hooks")
	}
	if code, _, _ := runApp(t, "px", "shell-init", "tcsh"); code != 2 {
		t.Fatal("tcsh accepted")
	}
	if _, err := os.Stat(paths.Home()); !os.IsNotExist(err) {
		t.Fatal("shell-init created the config home")
	}
	rc := filepath.Join(t.TempDir(), "rc")
	os.WriteFile(rc, []byte("# mine\n"), 0o644)
	if out := mustRun(t, "px", "shell", "install", "zsh", "--rc", rc, "--dry-run"); !strings.Contains(out, shell.BlockStart) {
		t.Fatalf("dry run:\n%s", out)
	}
	if data, _ := os.ReadFile(rc); string(data) != "# mine\n" {
		t.Fatal("dry run wrote")
	}
	_, _, errs := runApp(t, "px", "shell", "install", "zsh", "--rc", rc)
	data, _ := os.ReadFile(rc)
	snippet := filepath.Join(paths.ShellDir(), "init.zsh")
	if !strings.Contains(string(data), shell.BlockStart) || !strings.Contains(string(data), snippet) || !strings.Contains(errs, "backup") {
		t.Fatalf("install:\n%s\n%s", data, errs)
	}
	if s, err := os.ReadFile(snippet); err != nil || !strings.Contains(string(s), "__silkrouter_wrap") {
		t.Fatalf("snippet: %v", err)
	}
	if _, _, errs := runApp(t, "px", "shell", "install", "zsh", "--rc", rc); !strings.Contains(errs, "up to date") {
		t.Fatalf("reinstall: %s", errs)
	}
	if code, _, _ := runApp(t, "px", "shell", "install", "zsh", "bash", "--rc", rc); code != 2 {
		t.Fatal("--rc with two shells")
	}
	mustRun(t, "px", "shell", "uninstall", "zsh", "--rc", rc, "--no-backup")
	if data, _ := os.ReadFile(rc); string(data) != "# mine\n" {
		t.Fatalf("uninstall: %q", data)
	}
	if code, _, _ := runApp(t, "px", "shell", "unexpected-arg"); code != 2 {
		t.Fatal("px shell with args")
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	j := strings.Index(s, b)
	if j < 0 {
		return ""
	}
	return s[:j]
}
