package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestQuoteAndEmit(t *testing.T) {
	if QuoteSh("it's") != `'it'\''s'` || QuoteFish(`a'b\c`) != `'a\'b\\c'` {
		t.Fatal("quoting")
	}
	vars := []Var{{Name: "A", Value: "x y"}, {Name: "B", Unset: true}}
	if got := Emit("zsh", vars); got != "export A='x y'\nunset B\n" {
		t.Fatalf("sh: %q", got)
	}
	if got := Emit("fish", vars); got != "set -gx A 'x y'\nset -e B\n" {
		t.Fatalf("fish: %q", got)
	}
}

func TestOnOffVars(t *testing.T) {
	s := &Session{ID: "abc", Proxy: "socks5://u:p@192.0.2.1:1080", Key: "socks5://192.0.2.1:1080", ProxyID: 42, MS: 120,
		Country: "PL", Started: time.Unix(1700000000, 0)}
	before := map[string]string{"HTTP_PROXY": "http://old:3128", "PATH": "/bin"}
	on := OnVars(s, "localhost", "*", "208", "{icon} {scheme} {host}:{port} {ms} {cc}", true, "socks5", "192.0.2.1", 1080, env(before))
	m := map[string]string{}
	for _, v := range on {
		m[v.Name] = v.Value
	}
	if m["ALL_PROXY"] != s.Proxy || m["https_proxy"] != s.Proxy || m["NO_PROXY"] != "localhost" || m[EnvSession] != "abc" ||
		m[EnvActive] != "1" || m[EnvProxyID] != "42" || m[EnvLog] != "1" || m["_SILKROUTER_SAVED_HTTP_PROXY"] != "http://old:3128" {
		t.Fatalf("on vars: %v", m)
	}
	if m[EnvPrompt] != "* socks5 192.0.2.1:1080 120ms PL" || m[EnvPromptText] != "socks5 192.0.2.1:1080 120ms PL" {
		t.Fatalf("prompt: %q / %q", m[EnvPrompt], m[EnvPromptText])
	}
	// Already active: nothing saved again (rotate keeps the original backup).
	m2 := map[string]string{EnvActive: "1", "HTTP_PROXY": s.Proxy}
	for _, v := range OnVars(s, "", "", "", "{host}", false, "socks5", "192.0.2.1", 1080, env(m2)) {
		if strings.HasPrefix(v.Name, savedPrefix) {
			t.Fatal("saved twice")
		}
	}
	// off restores the saved HTTP_PROXY and unsets everything else.
	off := OffVars(env(map[string]string{"_SILKROUTER_SAVED_HTTP_PROXY": "http://old:3128", EnvActive: "1"}))
	restored, unsetActive := false, false
	for _, v := range off {
		if v.Name == "HTTP_PROXY" && !v.Unset && v.Value == "http://old:3128" {
			restored = true
		}
		if v.Name == EnvActive && v.Unset {
			unsetActive = true
		}
		if v.Name == "ALL_PROXY" && !v.Unset {
			t.Fatal("ALL_PROXY kept")
		}
	}
	if !restored || !unsetActive {
		t.Fatalf("off: %+v", off)
	}
	envl := EnvList([]string{"PATH=/bin", "ALL_PROXY=old"}, on)
	joined := strings.Join(envl, "\n")
	if !strings.Contains(joined, "PATH=/bin") || strings.Contains(joined, "ALL_PROXY=old") || !strings.Contains(joined, "ALL_PROXY="+s.Proxy) {
		t.Fatalf("env list: %v", envl)
	}
}

func TestRenderPrompt(t *testing.T) {
	f := PromptFields{Icon: "@", Scheme: "http", Host: "h", Port: 8080, MS: 0, Country: "", ID: 7, Tags: []string{"fast", "elite"}}
	if got := RenderPrompt("{icon} {scheme} {host}:{port} {ms} {cc} {id} {tags}", f); got != "@ http h:8080 #7 fast,elite" {
		t.Fatalf("%q", got)
	}
	if got := RenderPrompt("{host}:{port}", PromptFields{}); got != "" {
		t.Fatalf("empty: %q", got)
	}
}

func TestStore(t *testing.T) {
	st := Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	s := &Session{ID: NewID(), Proxy: "http://192.0.2.1:80", Started: time.Now().UTC(), Kind: "on"}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(&Session{ID: "../x"}); err == nil {
		t.Fatal("bad id accepted")
	}
	got, err := st.Load(s.ID)
	if err != nil || got.Proxy != s.Proxy || !got.Active() {
		t.Fatalf("%+v %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(st.Dir, s.ID+".json")); fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("mode %v", fi.Mode())
	}
	old := &Session{ID: "old1", Started: time.Now().Add(-48 * time.Hour), Ended: time.Now().Add(-47 * time.Hour)}
	st.Save(old)
	list, _ := st.List()
	if len(list) != 2 || list[0].ID != s.ID {
		t.Fatalf("list: %+v", list)
	}
	if _, err := st.End(s.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Load(s.ID); got.Active() {
		t.Fatal("ended session active")
	}
	if n, err := st.Prune(time.Now().Add(-24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
}

func TestInitScripts(t *testing.T) {
	for _, sh := range Shells {
		out, err := Init(sh, InitOptions{Bin: "/usr/bin/silkrouter", Hooks: true, P10K: "left", PromptAuto: true, Version: "4.0.0"})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"__silkrouter_wrap", "--emit " + sh, "__shell-log", "/usr/bin/silkrouter"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s init lacks %q", sh, want)
			}
		}
		for _, n := range []string{"px", "s-proxy", "silkrouter"} {
			if !strings.Contains(out, n+"()") && !strings.Contains(out, "function "+n) {
				t.Errorf("%s init lacks function %s", sh, n)
			}
		}
		bare, _ := Init(sh, InitOptions{Bin: "x", P10K: "none"})
		if strings.Contains(bare, "__shell-log") || strings.Contains(bare, "p10k reload") {
			t.Errorf("%s: hooks / p10k setup without asking", sh)
		}
	}
	zsh, _ := Init("zsh", InitOptions{Bin: "x", P10K: "right"})
	if !strings.Contains(zsh, "prompt_silkrouter") || !strings.Contains(zsh, "POWERLEVEL9K_RIGHT_PROMPT_ELEMENTS=(silkrouter") {
		t.Fatal("zsh p10k segment")
	}
	if _, err := Init("tcsh", InitOptions{}); err == nil {
		t.Fatal("tcsh accepted")
	}
	// Syntax check with the real shells when they are installed.
	for _, sh := range Shells {
		bin, err := exec.LookPath(sh)
		if err != nil {
			continue
		}
		out, _ := Init(sh, InitOptions{Bin: "/usr/bin/silkrouter", Hooks: true, P10K: "left", PromptAuto: true})
		f := filepath.Join(t.TempDir(), "init."+sh)
		os.WriteFile(f, []byte(out), 0o600)
		if msg, err := exec.Command(bin, "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s -n: %v\n%s", sh, err, msg)
		}
	}
}

func TestRCBlock(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	os.WriteFile(rc, []byte("export FOO=1\n"), 0o644)
	block := Block("zsh", "/home/u/.config/szmelc/proxy/shell/init.zsh", "silkrouter")
	res, err := Install(rc, block, false, false)
	if err != nil || !res.Changed || res.Existed || res.Backup == "" {
		t.Fatalf("install: %+v %v", res, err)
	}
	data, _ := os.ReadFile(rc)
	if !strings.HasPrefix(string(data), "export FOO=1\n\n"+BlockStart) || strings.Count(string(data), BlockStart) != 1 {
		t.Fatalf("rc:\n%s", data)
	}
	// Idempotent; a changed block is replaced in place.
	if res, _ := Install(rc, block, false, false); res.Changed {
		t.Fatal("second install changed the file")
	}
	os.WriteFile(rc, []byte(string(data)+"alias x=y\n"), 0o644)
	res, _ = Install(rc, Block("zsh", "/other/init.zsh", "silkrouter"), true, false)
	data, _ = os.ReadFile(rc)
	if !res.Existed || !strings.Contains(string(data), "/other/init.zsh") || !strings.HasSuffix(string(data), "alias x=y\n") {
		t.Fatalf("refresh:\n%s", data)
	}
	// Dry run changes nothing.
	if res, _ := Uninstall(rc, true, true); !res.Changed {
		t.Fatal("dry run")
	}
	if d2, _ := os.ReadFile(rc); string(d2) != string(data) {
		t.Fatal("dry run wrote")
	}
	if _, err := Uninstall(rc, true, false); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(rc)
	if string(data) != "export FOO=1\n\nalias x=y\n" {
		t.Fatalf("uninstall:\n%q", data)
	}
	fish := Block("fish", "/s/init.fish", "silkrouter")
	if !strings.Contains(fish, "source '/s/init.fish'") || !strings.Contains(fish, "shell-init fish | source") {
		t.Fatalf("fish block:\n%s", fish)
	}
}

func TestBootstrap(t *testing.T) {
	dir := t.TempDir()
	args, env, err := Bootstrap("zsh", filepath.Join(dir, "z"), "/s/init.zsh")
	if err != nil || len(args) == 0 || len(env) != 1 || !strings.HasPrefix(env[0], "ZDOTDIR=") {
		t.Fatalf("zsh: %v %v %v", args, env, err)
	}
	rc, _ := os.ReadFile(filepath.Join(dir, "z", ".zshrc"))
	if !strings.Contains(string(rc), ".zshrc") || !strings.Contains(string(rc), "/s/init.zsh") {
		t.Fatalf("zshrc:\n%s", rc)
	}
	args, _, _ = Bootstrap("bash", filepath.Join(dir, "b"), "/s/init.bash")
	if args[0] != "--rcfile" {
		t.Fatalf("bash: %v", args)
	}
	args, _, _ = Bootstrap("fish", dir, "/s/init.fish")
	if !strings.Contains(strings.Join(args, " "), "source '/s/init.fish'") {
		t.Fatalf("fish: %v", args)
	}
	if RCFile("bash", "/h") != filepath.Join("/h", ".bashrc") || Supported("tcsh") || !Supported("fish") {
		t.Fatal("rc file / supported")
	}
}

func TestRenderPromptStripsShellSyntax(t *testing.T) {
	// Country codes and tags come from untrusted sources and end up in PS1.
	f := PromptFields{Icon: "*", Scheme: "socks5", Host: "1.2.3.4", Port: 1080,
		Country: "$(touch /tmp/pwned)`id`", Tags: []string{"fast", "$x"}}
	got := RenderPrompt("{icon} {scheme} {host}:{port} {cc} {tags}", f)
	if want := "* socks5 1.2.3.4:1080 touchtmppwnedid fast,x"; got != want {
		t.Fatalf("RenderPrompt = %q, want %q", got, want)
	}
}
