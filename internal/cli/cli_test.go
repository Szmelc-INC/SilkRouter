package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/paths"
)

// recorder captures what a fake command received.
type recorder struct {
	path    string
	args    []string
	legacy  bool
	quiet   bool
	verbose bool
	maxMS   int
	timeout config.Duration
	flagX   bool
}

// testApp builds an isolated registry that mirrors the real command shapes.
func testApp(t *testing.T) (*App, *recorder, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv(paths.EnvHome, filepath.Join(t.TempDir(), "home"))
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} { // os.TempDir() on every platform
		t.Setenv(k, tmp)
	}
	t.Setenv("PX_LIST", "")
	t.Setenv("NO_COLOR", "1")

	rec := &recorder{}
	run := func(ctx *Context, args []string) int {
		rec.path = strings.Join(ctx.Path, " ")
		rec.args = args
		rec.legacy = ctx.Legacy
		rec.quiet = ctx.Quiet
		rec.verbose = ctx.Verbose
		return 0
	}
	r := NewRegistry()
	r.Fallback = "route"
	r.LegacyModes = map[string]LegacyMode{
		"-B":     {Command: "build"},
		"-L":     {Command: "list"},
		"--list": {Command: "list"},
		"-F":     {Command: "build", Forward: "-F", TakesValue: true},
		"--file": {Command: "build", Forward: "-F", TakesValue: true},
	}
	r.Register(Command{
		Name: "route", Aliases: []string{"run"}, Group: GroupRoute,
		Summary: "run a command through a proxy", Usage: "[options] [--] <command...>",
		Args: ArgsCommand, PassThrough: true, NeedsHome: true,
		Flags: func(fs *FlagSet) {
			fs.Int(&rec.maxMS, "d,max-ms", fs.Config.Route.MaxMS, "max latency").Placeholder("MS").ConfigKey("route.max_ms")
			fs.Bool(&rec.flagX, "s,socks", false, "socks only")
			fs.Bool(&rec.verbose, "v,verbose", false, "print exit IP")
			fs.Duration(&rec.timeout, "t,timeout", fs.Config.Route.VerifyTimeout, "verify timeout")
			fs.String(new(string), "probe", "cloudflare", "probe").Dynamic("probes")
			fs.String(new(string), "f,cache", "/x/cache.list", "cache file").Files()
		},
		Examples: []Example{{Cmd: "curl ifconfig.me", Desc: "legacy style"}},
		Run: func(ctx *Context, args []string) int {
			v := rec.verbose
			code := run(ctx, args)
			rec.verbose = v || ctx.Verbose
			return code
		},
	})
	r.Register(Command{
		Name: "build", Group: GroupCache, Summary: "build the cache", NeedsHome: true,
		Flags: func(fs *FlagSet) {
			fs.String(new(string), "F,from-file", "", "source file").Files()
			fs.Bool(new(bool), "builtin", false, "embedded list")
		},
		Run: run,
	})
	r.Register(Command{Name: "list", Aliases: []string{"ls"}, Group: GroupCache, Summary: "list the cache", Run: run})
	r.Register(Command{
		Name: "check", Group: GroupCheck, Summary: "check a proxy", Args: ArgsFiles,
		Flags: func(fs *FlagSet) {
			fs.Bool(&rec.flagX, "x", false, "a bool")
			fs.Duration(&rec.timeout, "t,timeout", config.Seconds(8), "timeout")
		},
		Run: run,
	})
	r.Register(Command{
		Name: "config", Group: GroupConfig, Summary: "configuration",
		Subcommands: []Command{
			{Name: "get", Summary: "get a key", Args: ArgsDynamic("config-keys"), Run: run},
			{Name: "set", Summary: "set a key", Run: run, NeedsHome: true},
			{Name: "show", Aliases: []string{"cat"}, Summary: "show", Run: run,
				Flags: func(fs *FlagSet) { fs.Bool(new(bool), "json", false, "JSON") }},
		},
	})
	r.Register(Command{Name: "completion", Group: GroupMeta, Summary: "completion scripts", Args: ArgsChoices(Shells...), Run: run})
	r.Register(Command{Name: "__complete", Hidden: true, Run: run})

	var out, errb bytes.Buffer
	app := &App{
		Registry: r, Name: "SilkRouter", Binary: "silkrouter", Aliases: []string{"px", "s-proxy"},
		Version: "9.9.9", Tagline: "test", Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""),
	}
	return app, rec, &out, &errb
}

func TestResolve(t *testing.T) {
	app, _, _, _ := testApp(t)
	cases := []struct {
		args   []string
		path   string
		rest   []string
		legacy bool
	}{
		{[]string{"route", "-d", "5", "curl"}, "route", []string{"-d", "5", "curl"}, false},
		{[]string{"run", "curl"}, "route", []string{"curl"}, false},
		{[]string{"ls"}, "list", []string{}, false},
		{[]string{"config", "set", "a", "b"}, "config set", []string{"a", "b"}, false},
		{[]string{"config", "cat", "--json"}, "config show", []string{"--json"}, false},
		{[]string{"config"}, "config", []string{}, false},
		// Legacy px.sh style: unknown first word routes everything.
		{[]string{"curl", "ifconfig.me"}, "route", []string{"curl", "ifconfig.me"}, true},
		{[]string{"-d", "500", "-s", "curl", "x"}, "route", []string{"-d", "500", "-s", "curl", "x"}, true},
		// "--" always routes, even a subcommand's name, and is kept.
		{[]string{"--", "build", "-x"}, "route", []string{"--", "build", "-x"}, false},
		// px.sh mode flags.
		{[]string{"-B", "-j", "5"}, "build", []string{"-j", "5"}, true},
		{[]string{"-L"}, "list", []string{}, true},
		{[]string{"--list"}, "list", []string{}, true},
		{[]string{"-F", "f.txt", "-O"}, "build", []string{"-F", "f.txt", "-O"}, true},
		{[]string{"--file=f.txt"}, "build", []string{"-F", "f.txt"}, true},
	}
	for _, c := range cases {
		res, err := app.Registry.Resolve(c.args)
		if err != nil {
			t.Errorf("Resolve(%q): %v", c.args, err)
			continue
		}
		if res.PathString() != c.path || res.Legacy != c.legacy || !reflect.DeepEqual(append([]string{}, res.Args...), c.rest) {
			t.Errorf("Resolve(%q) = %q %q legacy=%v, want %q %q legacy=%v",
				c.args, res.PathString(), res.Args, res.Legacy, c.path, c.rest, c.legacy)
		}
	}

	if res, err := app.Registry.Resolve(nil); err != nil || res.Command != nil {
		t.Fatalf("Resolve(nil) = %+v, %v", res, err)
	}
	_, err := app.Registry.Resolve([]string{"config", "sett"})
	if err == nil || !strings.Contains(err.Error(), `did you mean set`) {
		t.Fatalf("unknown subcommand error = %v", err)
	}
	if _, err := app.Registry.Resolve([]string{"-F"}); err == nil {
		t.Fatal("-F without a value should fail")
	}
}

func TestParseGlobals(t *testing.T) {
	cases := []struct {
		args []string
		rest []string
		want Globals
	}{
		{[]string{"--no-color", "-q", "build"}, []string{"build"}, Globals{NoColor: true, Quiet: true}},
		{[]string{"--config", "a.toml", "-home=/h", "x"}, []string{"x"}, Globals{Config: "a.toml", Home: "/h"}},
		{[]string{"-config=b.toml", "--verbose"}, []string{}, Globals{Config: "b.toml", Verbose: true}},
		{[]string{"-v", "-d", "500", "curl"}, []string{"-d", "500", "curl"}, Globals{Verbose: true}},
		{[]string{"-q", "--", "build"}, []string{"--", "build"}, Globals{Quiet: true}},
		{[]string{"--help", "route"}, []string{"route"}, Globals{Help: true}},
		{[]string{"-B"}, []string{"-B"}, Globals{}},
		{[]string{"--quiet=false", "list"}, []string{"list"}, Globals{}},
	}
	for _, c := range cases {
		var g Globals
		rest, err := ParseGlobals(c.args, &g)
		if err != nil {
			t.Errorf("ParseGlobals(%q): %v", c.args, err)
			continue
		}
		if !reflect.DeepEqual(append([]string{}, rest...), c.rest) || g != c.want {
			t.Errorf("ParseGlobals(%q) = %q %+v, want %q %+v", c.args, rest, g, c.rest, c.want)
		}
	}
	var g Globals
	if _, err := ParseGlobals([]string{"--config"}, &g); err == nil {
		t.Fatal("--config without a value should fail")
	}
}

func TestProgName(t *testing.T) {
	app, _, _, _ := testApp(t)
	cases := map[string]string{
		"px":                      "px",
		"/usr/bin/px":             "px",
		"s-proxy":                 "s-proxy",
		"/opt/x/s-proxy.exe":      "s-proxy",
		`C:\tools\SILKROUTER.EXE`: "silkrouter",
		"silkrouter":              "silkrouter",
		"silkrouter-linux-amd64":  "silkrouter",
		"/tmp/go-build123/main":   "silkrouter",
	}
	for in, want := range cases {
		if got := app.ProgName(in); got != want {
			t.Errorf("ProgName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArgv0Identical(t *testing.T) {
	var outputs []string
	for _, argv0 := range []string{"px", "/usr/local/bin/s-proxy", "silkrouter.exe"} {
		app, rec, out, _ := testApp(t)
		if code := app.Run(context.Background(), []string{argv0, "-v", "check", "p.txt", "-t", "3"}); code != 0 {
			t.Fatalf("%s: exit %d", argv0, code)
		}
		outputs = append(outputs, rec.path+"|"+strings.Join(rec.args, " ")+"|"+rec.timeout.String())
		if !rec.verbose {
			t.Errorf("%s: global -v lost", argv0)
		}
		out.Reset()
		app.Run(context.Background(), []string{argv0, "--help"})
		if !strings.Contains(out.String(), app.ProgName(argv0)+" [global options]") {
			t.Errorf("%s: help does not use the invoked name:\n%s", argv0, out.String())
		}
	}
	for _, o := range outputs[1:] {
		if o != outputs[0] {
			t.Fatalf("argv[0] changed behaviour: %q vs %q", o, outputs[0])
		}
	}
}

func TestDispatch(t *testing.T) {
	app, rec, out, errb := testApp(t)
	ctx := context.Background()
	run := func(args ...string) int {
		out.Reset()
		errb.Reset()
		*rec = recorder{}
		return app.Run(ctx, append([]string{"px"}, args...))
	}

	// Legacy fallthrough parses route flags, then stops at the program.
	if code := run("-v", "-d", "500", "-s", "curl", "-x", "ifconfig.me"); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if rec.path != "route" || !rec.legacy || rec.maxMS != 500 || !rec.flagX || !rec.verbose ||
		!reflect.DeepEqual(rec.args, []string{"curl", "-x", "ifconfig.me"}) {
		t.Fatalf("legacy route: %+v", rec)
	}

	// "--" routes a program named like a subcommand, flags untouched.
	run("--", "build", "--help")
	if rec.path != "route" || !reflect.DeepEqual(rec.args, []string{"build", "--help"}) {
		t.Fatalf("-- routing: %+v", rec)
	}

	// Interspersed flags for normal commands; globals after the command.
	run("check", "proxy.txt", "-x", "-t", "2.5", "-q")
	if rec.path != "check" || !rec.flagX || rec.timeout.String() != "2.5s" || !rec.quiet ||
		!reflect.DeepEqual(rec.args, []string{"proxy.txt"}) {
		t.Fatalf("check: %+v", rec)
	}
	run("check", "-x", "--", "-weird-name")
	if !reflect.DeepEqual(rec.args, []string{"-weird-name"}) {
		t.Fatalf("-- in check: %q", rec.args)
	}

	// Single-dash long flags keep working, double-dash short ones too.
	run("route", "-max-ms", "7", "--s", "true")
	if rec.maxMS != 7 || !rec.flagX {
		t.Fatalf("flag spellings: %+v", rec)
	}

	// Config defaults feed flag defaults; env beats the file.
	t.Setenv("SILKROUTER_ROUTE_MAX_MS", "321")
	run("route", "true")
	if rec.maxMS != 321 {
		t.Fatalf("env default not applied: %d", rec.maxMS)
	}

	// -q wins over -v.
	run("-q", "-v", "list")
	if !rec.quiet || rec.verbose {
		t.Fatalf("quiet/verbose: %+v", rec)
	}

	// Help paths exit 0 and print to stdout.
	for _, args := range [][]string{{"route", "--help"}, {"route", "-h"}, {"--help", "route"}, {"route", "-help"}} {
		if code := run(args...); code != 0 || !strings.Contains(out.String(), "px route") {
			t.Fatalf("%q: exit %d\n%s", args, code, out.String())
		}
	}
	if code := run("config"); code != 0 || !strings.Contains(out.String(), "COMMANDS") {
		t.Fatalf("group without subcommand: %d\n%s", code, out.String())
	}

	// Errors exit 2 with a hint.
	if code := run("check", "--nope"); code != 2 || !strings.Contains(errb.String(), "px check --help") {
		t.Fatalf("bad flag: %d %s", code, errb.String())
	}
	if code := run("config", "nope"); code != 2 || !strings.Contains(errb.String(), "px config --help") {
		t.Fatalf("bad subcommand: %d %s", code, errb.String())
	}
	if code := run(); code != 2 || !strings.Contains(errb.String(), "USAGE") {
		t.Fatalf("no args: %d", code)
	}
	if code := run("--version"); code != 0 || out.String() != "silkrouter 9.9.9\n" {
		t.Fatalf("--version: %d %q", code, out.String())
	}
}

func TestNeedsHome(t *testing.T) {
	app, _, _, errb := testApp(t)
	home := paths.Home()
	app.Run(context.Background(), []string{"px", "list"})
	if _, err := os.Stat(home); err == nil {
		t.Fatal("a command without NeedsHome created the config home")
	}
	setup := 0
	app.Setup = func(*Context) error { setup++; return nil }
	if code := app.Run(context.Background(), []string{"px", "config", "set", "a", "b"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, d := range paths.Dirs() {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("NeedsHome did not create %s", d)
		}
	}
	if setup != 1 {
		t.Fatalf("Setup ran %d times", setup)
	}
}

func TestHomeFlag(t *testing.T) {
	app, _, out, _ := testApp(t)
	dir := t.TempDir()
	t.Setenv(paths.EnvHome, "") // restored after the test
	app.Run(context.Background(), []string{"px", "--home", dir, "--help"})
	if paths.Home() != dir {
		t.Fatalf("--home not applied: %s", paths.Home())
	}
	cfg := filepath.Join(dir, "config.toml")
	if !strings.Contains(out.String(), cfg) && !strings.Contains(out.String(), paths.Pretty(cfg)) {
		t.Fatalf("help should show the config path under --home:\n%s", out.String())
	}
}

func TestConfigWarningsPrinted(t *testing.T) {
	app, _, _, errb := testApp(t)
	cfg := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(cfg, []byte("[route]\nbogus = 1\n"), 0o600)
	app.Run(context.Background(), []string{"px", "--config", cfg, "list"})
	if !strings.Contains(errb.String(), "unknown key") {
		t.Fatalf("config warning not shown: %q", errb.String())
	}
	errb.Reset()
	app.Run(context.Background(), []string{"px", "--config", cfg, "-q", "list"})
	if errb.Len() != 0 {
		t.Fatalf("-q should hide config warnings: %q", errb.String())
	}
	app.Run(context.Background(), []string{"px", "--config", cfg, "list", "-q"})
	if errb.Len() != 0 {
		t.Fatalf("-q after the command should hide config warnings: %q", errb.String())
	}
	errb.Reset()
	app.Run(context.Background(), []string{"px", "--config", cfg, "__complete", "x"})
	if strings.Contains(errb.String(), "unknown key") {
		t.Fatalf("hidden commands must not print config warnings: %q", errb.String())
	}
}

func TestHelpRendering(t *testing.T) {
	app, _, out, _ := testApp(t)
	app.WriteHelp(out, "px", config.Default())
	main := out.String()
	for _, want := range []string{"SilkRouter 9.9.9", "ROUTING", "CACHE", "route, run", "list, ls", "--no-color", "-q, --quiet", "--config FILE"} {
		if !strings.Contains(main, want) {
			t.Errorf("main help misses %q:\n%s", want, main)
		}
	}
	if strings.Contains(main, "__complete") {
		t.Error("hidden command listed in help")
	}
	if strings.Contains(main, "\033[") {
		t.Error("NO_COLOR help contains escape codes")
	}
	if strings.Index(main, "ROUTING") > strings.Index(main, "CACHE") {
		t.Error("groups not in the declared order")
	}

	out.Reset()
	cfg := config.Default()
	app.WriteCommandHelp(out, "px", []string{"route"}, cfg)
	h := out.String()
	for _, want := range []string{"px route [options] [--] <command...>", "-d, --max-ms MS", "default: 1000", "config: route.max_ms", "    --probe", "aliases: run", "px curl ifconfig.me"} {
		if !strings.Contains(h, want) {
			t.Errorf("route help misses %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "-v, --verbose        print more") {
		t.Error("global -v shown although route defines its own -v")
	}
}

func TestSuggest(t *testing.T) {
	app, _, _, _ := testApp(t)
	if s := app.Registry.Suggest("biuld"); len(s) == 0 || s[0] != "build" {
		t.Fatalf("Suggest(biuld) = %v", s)
	}
	if s := app.Registry.Suggest("curl"); len(s) != 0 {
		t.Fatalf("Suggest(curl) = %v", s)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	r := NewRegistry()
	r.Register(Command{Name: "a", Aliases: []string{"b"}})
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate alias did not panic")
		}
	}()
	r.Register(Command{Name: "b"})
}

func TestChanged(t *testing.T) {
	fs := NewFlagSet("x", nil)
	var a, b bool
	fs.Bool(&a, "a,alpha", false, "")
	fs.Bool(&b, "b", false, "")
	if _, err := fs.parse([]string{"--alpha"}, true); err != nil {
		t.Fatal(err)
	}
	if !fs.Changed("a") || !fs.Changed("alpha") || fs.Changed("b") {
		t.Fatal("Changed is wrong")
	}
}

// --- completion -------------------------------------------------------

func scripts(t *testing.T, app *App) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, sh := range Shells {
		var b bytes.Buffer
		if err := app.WriteCompletion(&b, sh); err != nil {
			t.Fatal(err)
		}
		out[sh] = b.String()
	}
	return out
}

func TestCompletionContents(t *testing.T) {
	app, _, _, _ := testApp(t)
	for sh, s := range scripts(t, app) {
		for _, c := range app.Registry.Visible() {
			for _, n := range c.Names() {
				if !strings.Contains(s, n) {
					t.Errorf("%s script misses command %q", sh, n)
				}
			}
		}
		for _, want := range []string{"max-ms", "from-file", "show", "cat", "__complete", "px", "s-proxy", "silkrouter"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s script misses %q", sh, want)
			}
		}
	}
	if err := app.WriteCompletion(&bytes.Buffer{}, "tcsh"); err == nil {
		t.Fatal("unknown shell accepted")
	}
}

// checkCompletionSyntax runs `<shell> -n` on every generated script when the
// shell is installed.
func checkCompletionSyntax(t *testing.T, app *App) {
	t.Helper()
	dir := t.TempDir()
	for sh, s := range scripts(t, app) {
		bin, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s not installed; skipping its syntax check", sh)
			continue
		}
		f := filepath.Join(dir, "completion."+sh)
		if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(bin, "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s -n: %v\n%s", sh, err, out)
		}
	}
}

func TestCompletionSyntax(t *testing.T) {
	app, _, _, _ := testApp(t)
	checkCompletionSyntax(t, app)
}

// TestBashCompletionBehaviour drives the generated bash function with
// simulated COMP_WORDS.
func TestBashCompletionBehaviour(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	app, _, _, _ := testApp(t)
	var b bytes.Buffer
	if err := app.WriteCompletion(&b, "bash"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "c.bash")
	os.WriteFile(script, b.Bytes(), 0o600)

	cases := []struct {
		line string // words; the last one is being completed
		want []string
		not  []string
	}{
		{"px ''", []string{"route", "run", "build", "config", "ls"}, []string{"__complete"}},
		{"px ro", []string{"route"}, []string{"build"}},
		{"px config ''", []string{"get", "set", "show", "cat"}, []string{"route"}},
		{"px config show --j", []string{"--json"}, nil},
		{"px route --m", []string{"--max-ms"}, nil},
		{"px -d 500 --s", []string{"--socks"}, nil},
		{"px --no-color --c", []string{"--config"}, nil},
		{"px completion ''", []string{"zsh", "bash", "fish"}, nil},
		{"px completion zsh ''", nil, []string{"zsh"}},
		{"px -B --b", []string{"--builtin"}, nil},
	}
	for _, c := range cases {
		driver := filepath.Join(dir, "drive.bash")
		src := "source " + script + "\nCOMP_WORDS=(" + c.line + ")\nCOMP_CWORD=$((${#COMP_WORDS[@]} - 1))\n_silkrouter\nprintf '%s\\n' \"${COMPREPLY[@]}\"\n"
		os.WriteFile(driver, []byte(src), 0o600)
		out, err := exec.Command(bash, "--norc", "--noprofile", driver).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.line, err, out)
		}
		got := strings.Fields(string(out))
		for _, w := range c.want {
			if !contains(got, w) {
				t.Errorf("%q: missing %q in %v", c.line, w, got)
			}
		}
		for _, w := range c.not {
			if contains(got, w) {
				t.Errorf("%q: unexpected %q in %v", c.line, w, got)
			}
		}
	}
}

// TestFishCompletionBehaviour uses fish's `complete -C`.
func TestFishCompletionBehaviour(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not installed")
	}
	app, _, _, _ := testApp(t)
	var b bytes.Buffer
	if err := app.WriteCompletion(&b, "fish"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "c.fish")
	os.WriteFile(script, b.Bytes(), 0o600)
	cases := []struct {
		line string
		want []string
		not  []string
	}{
		{"px ", []string{"route", "build", "config", "ls"}, []string{"__complete"}},
		{"px config ", []string{"get", "set", "show"}, []string{"route"}},
		{"px route --", []string{"--max-ms", "--socks"}, nil},
		{"px -d 500 --", []string{"--max-ms"}, nil},
		{"px completion ", []string{"zsh", "bash", "fish"}, nil},
		{"px -L --", []string{"--help"}, []string{"--max-ms"}},
		{"px -F list.txt --", []string{"--builtin"}, nil},
		// The routed program's own completion takes over (without errors).
		{"px -- ls --al", nil, []string{"--max-ms"}},
		{"px -s ls --al", nil, []string{"--socks"}},
	}
	for _, c := range cases {
		driver := filepath.Join(dir, "drive.fish")
		src := "source " + script + "\ncomplete --do-complete='" + c.line + "'\n"
		os.WriteFile(driver, []byte(src), 0o600)
		cmd := exec.Command(fish, "--no-config", driver)
		cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir, "XDG_DATA_HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.line, err, out)
		}
		if strings.Contains(string(out), "complete:") || strings.Contains(string(out), "fish:") {
			t.Fatalf("%q: fish reported an error:\n%s", c.line, out)
		}
		var got []string
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.SplitN(l, "\t", 2); f[0] != "" {
				got = append(got, f[0])
			}
		}
		for _, w := range c.want {
			if !contains(got, w) {
				t.Errorf("%q: missing %q in %v", c.line, w, got)
			}
		}
		for _, w := range c.not {
			if contains(got, w) {
				t.Errorf("%q: unexpected %q in %v", c.line, w, got)
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
