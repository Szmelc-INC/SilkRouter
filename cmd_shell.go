package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/history"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/router"
	"github.com/szmelc-inc/silkrouter/internal/shell"
)

// selOpts are the proxy selection flags of on / rotate / shell (a subset
// of route's). Exported fields: they are stored in the session for rotate.
type selOpts struct {
	Socks    bool            `json:"socks,omitempty"`
	HTTP     bool            `json:"http,omitempty"`
	MaxMS    int             `json:"max_ms,omitempty"`
	MinMS    int             `json:"min_ms,omitempty"`
	Force    string          `json:"-"`
	Tag      string          `json:"tag,omitempty"`
	Exclude  string          `json:"exclude,omitempty"`
	Best     bool            `json:"best,omitempty"`
	For      string          `json:"for,omitempty"`
	Prefer   string          `json:"prefer,omitempty"`
	NoVerify bool            `json:"no_verify,omitempty"`
	Timeout  config.Duration `json:"timeout,omitempty"`
	Probe    string          `json:"probe,omitempty"`
}

var shOpts struct {
	sel       selOpts
	emit      string
	print     bool
	json      bool
	noHooks   bool
	noPrompt  bool
	p10k      string
	write     bool
	bin       string
	shell     string
	chain     bool
	method    string
	rc        string
	dryRun    bool
	noBackup  bool
	format    string
	plain     bool
	all       bool
	olderThan string
	relay     bool
	relayAddr string
}

func selFlags(fs *cli.FlagSet, s *selOpts) {
	cfg := fs.Config
	fs.Bool(&s.Socks, "s,socks", cfg.Route.Scheme == "socks", "socks proxies only").ConfigKey("route.scheme")
	fs.Bool(&s.HTTP, "H,http", cfg.Route.Scheme == "http", "http/https proxies only").ConfigKey("route.scheme")
	fs.Int(&s.MaxMS, "d,max-ms", cfg.Route.MaxMS, "maximum last latency (0 = no limit)").Placeholder("MS").ConfigKey("route.max_ms")
	fs.Int(&s.MinMS, "D,min-ms", cfg.Route.MinMS, "minimum last latency").Placeholder("MS").ConfigKey("route.min_ms")
	fs.String(&s.Force, "p,proxy", "", "use this proxy").Placeholder("URL")
	fs.String(&s.Tag, "T,tag", "", "only proxies with one of these tags").Placeholder("TAGS").Dynamic("tags")
	fs.String(&s.Exclude, "exclude-tag", strings.Join(cfg.Route.ExcludeTags, ","), "never use proxies with these tags").
		Placeholder("TAGS").Dynamic("tags").ConfigKey("route.exclude_tags")
	fs.Bool(&s.Best, "b,best", false, "best ranked first")
	fs.String(&s.For, "for", "", "prefer the recommendations / favourites of this command").Placeholder("CMD").Dynamic("used-commands")
	fs.String(&s.Prefer, "prefer", cfg.Route.Prefer, "pick order").Placeholder("ORDER").Choices("smart", "rank", "random").
		ConfigKey("route.prefer")
	fs.Bool(&s.NoVerify, "x,no-verify", !cfg.Shell.Verify, "do not verify the proxy first").ConfigKey("shell.verify")
	fs.Duration(&s.Timeout, "t,timeout", cfg.Route.VerifyTimeout, "timeout of one verification").ConfigKey("route.verify_timeout")
	fs.String(&s.Probe, "probe", cfg.Probe.Name, "verification probe").Placeholder("NAME").Dynamic("probes").ConfigKey("probe.name")
}

func emitFlags(fs *cli.FlagSet) {
	fs.String(&shOpts.emit, "emit", "", "print shell code for this shell (used by the wrapper function)").
		Placeholder("SHELL").Choices(shell.Shells...)
	fs.Bool(&shOpts.print, "print", false, "print the shell code for $SHELL (eval \"$(px on --print)\")")
}

const shellIntro = `A program cannot change the environment of the shell that started it, so
'px on' prints shell code and the wrapper function defined by the shell
integration evals it. Load the integration once from your rc file:

  zsh:   eval "$(silkrouter shell-init zsh)"        (~/.zshrc)
  bash:  eval "$(silkrouter shell-init bash)"       (~/.bashrc)
  fish:  silkrouter shell-init fish | source        (~/.config/fish/config.fish)

or let 'px shell install' add a marked block for you. Without it, use
eval "$(px on --print)" / eval "$(px off --print)", or 'px shell', which
starts a proxied subshell with everything loaded.`

func init() {
	cli.Register(cli.Command{
		Name:    "on",
		Group:   cli.GroupShell,
		Summary: "proxy this shell: every command (and its whole process tree) from now on",
		Description: `Picks a working proxy (like route: database pool, filters, tags,
--best / --for, verified first unless -x) and exports ALL_PROXY,
HTTP(S)_PROXY (both cases), NO_PROXY and the SILKROUTER_* session variables
in the current shell, so every command started from this terminal (and
everything those commands start) uses it. Previous proxy variables are
saved and restored by 'px off'. Running 'px on' again switches the proxy
inside the same session.

While it is on, the shell hooks log every command you type (mode shell, see
'px history --session $SILKROUTER_SESSION') and the prompt shows a segment
(powerlevel10k: 'silkrouter'; plain prompts get a prefix).

` + shellIntro,
		Usage:     "[selection options]",
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			selFlags(fs, &shOpts.sel)
			emitFlags(fs)
			fs.Bool(&shOpts.relay, "relay", false, "point the shell at the local relay (started in the background when needed) instead of one proxy")
			fs.String(&shOpts.relayAddr, "relay-addr", fs.Config.Relay.Listen, "relay address").Placeholder("ADDR").
				ConfigKey("relay.listen")
		},
		Examples: []cli.Example{
			{Cmd: "on", Desc: "a verified proxy (recommended / favourite first)"},
			{Cmd: "on -s -d 300", Desc: "a socks proxy under 300 ms"},
			{Cmd: "on --best --tag elite,clean"},
			{Cmd: "on -p socks5://127.0.0.1:9050", Desc: "a specific proxy (e.g. Tor)"},
			{Cmd: "on --relay", Desc: "stable 127.0.0.1 relay with failover / rotation underneath"},
		},
		Run: runOn,
	})
	cli.Register(cli.Command{
		Name:      "off",
		Group:     cli.GroupShell,
		Summary:   "stop proxying this shell (restore the previous variables)",
		NeedsHome: true,
		Flags:     func(fs *cli.FlagSet) { emitFlags(fs) },
		Run:       runOff,
	})
	cli.Register(cli.Command{
		Name:      "rotate",
		Group:     cli.GroupShell,
		Summary:   "switch this shell's session to another proxy (same filters)",
		Usage:     "[selection options]",
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			selFlags(fs, &shOpts.sel)
			emitFlags(fs)
		},
		Run: runRotate,
	})
	cli.Register(cli.Command{
		Name:    "session",
		Aliases: []string{"status", "sess"},
		Group:   cli.GroupShell,
		Summary: "the current shell session (proxy, age, commands logged); list / show / end",
		Flags: func(fs *cli.FlagSet) {
			fs.Bool(&shOpts.json, "json", false, "print as JSON")
		},
		NeedsHome: true,
		Run:       runSessionStatus,
		Subcommands: []cli.Command{
			{
				Name: "list", Aliases: []string{"ls"}, Summary: "list sessions (active first)", NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.Bool(&shOpts.all, "a,all", false, "also ended sessions")
					fs.Bool(&shOpts.json, "json", false, "print as JSON")
				},
				Run: runSessionList,
			},
			{
				Name: "show", Summary: "show one session and its commands", Usage: "<id>", NeedsHome: true,
				Flags: func(fs *cli.FlagSet) { fs.Bool(&shOpts.json, "json", false, "print as JSON") },
				Run:   runSessionShow,
			},
			{
				Name: "end", Summary: "mark a session ended (when its shell died)", Usage: "<id>...", NeedsHome: true,
				Run: runSessionEnd,
			},
			{
				Name: "prune", Summary: "delete ended session files", NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.String(&shOpts.olderThan, "older-than", "7d", "only sessions older than this").Placeholder("WHEN")
				},
				Run: runSessionPrune,
			},
		},
	})
	cli.Register(cli.Command{
		Name:    "shell",
		Aliases: []string{"subshell"},
		Group:   cli.GroupShell,
		Summary: "start a proxied subshell (its whole process tree goes through one proxy)",
		Description: `Starts your shell ($SHELL, or --shell) with a verified proxy exported, the
shell integration loaded on top of your own rc file (prompt segment, command
log) and its own session. Everything started from it uses the proxy; 'exit'
ends the session.

--chain (shell.method = proxychains) starts the shell under proxychains-ng
instead: every TCP connection of every process in the subshell is forced
through the proxy, also for programs that ignore the proxy variables.

'px shell install' / 'px shell uninstall' manage the integration block in
your rc files.`,
		Usage:     "[selection options]",
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			selFlags(fs, &shOpts.sel)
			fs.String(&shOpts.shell, "shell", "", "shell to start (default: $SHELL)").Placeholder("PROG")
			fs.Bool(&shOpts.chain, "chain,proxychains", false, "run the shell under proxychains (same as --method proxychains)")
			fs.String(&shOpts.method, "method", fs.Config.Shell.Method, "how the subshell is proxied").Placeholder("METHOD").
				Choices(router.MethodEnv, router.MethodProxychains).ConfigKey("shell.method")
		},
		Examples: []cli.Example{
			{Cmd: "shell"},
			{Cmd: "shell -s --best --chain", Desc: "best socks proxy, every connection forced through it"},
			{Cmd: "shell install", Desc: "add the integration to ~/.zshrc"},
		},
		Run: runSubshell,
		Subcommands: []cli.Command{
			{
				Name:    "install",
				Summary: "add the integration block to your rc files (with a backup)",
				Description: `Writes the generated init script to <home>/shell/init.<shell> and a marked
block that sources it to the rc file (~/.zshrc, ~/.bashrc,
~/.config/fish/config.fish; --rc to choose). Running it again refreshes both;
the previous rc file is kept as <rc>.silkrouter-bak-<time>.`,
				Usage:     "[options] [zsh|bash|fish...]",
				Args:      cli.ArgsChoices(shell.Shells...),
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.String(&shOpts.rc, "rc", "", "rc file to edit (one shell only)").Placeholder("FILE").Files()
					fs.Bool(&shOpts.dryRun, "n,dry-run", false, "only show what would change")
					fs.Bool(&shOpts.noBackup, "no-backup", false, "do not keep a copy of the rc file")
				},
				Run: runShellInstall,
			},
			{
				Name:      "uninstall",
				Summary:   "remove the integration block from your rc files",
				Usage:     "[options] [zsh|bash|fish...]",
				Args:      cli.ArgsChoices(shell.Shells...),
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.String(&shOpts.rc, "rc", "", "rc file to edit (one shell only)").Placeholder("FILE").Files()
					fs.Bool(&shOpts.dryRun, "n,dry-run", false, "only show what would change")
					fs.Bool(&shOpts.noBackup, "no-backup", false, "do not keep a copy of the rc file")
				},
				Run: runShellUninstall,
			},
		},
	})
	cli.Register(cli.Command{
		Name:    "shell-init",
		Aliases: []string{"init"},
		Group:   cli.GroupShell,
		Summary: "print the shell integration script (zsh, bash, fish)",
		Description: `Prints the script that defines the px / s-proxy / silkrouter wrapper
functions (so 'px on' / 'px off' / 'px rotate' can change the shell), the
preexec / precmd hooks that log commands typed while a proxy is on
(shell.log_commands) and the prompt integration: a powerlevel10k segment
named 'silkrouter' (added automatically per shell.p10k) and a prefix for
plain prompts (shell.prompt_auto). The segment only reads environment
variables, so p10k instant prompt keeps working.

` + shellIntro,
		Usage: "[options] <zsh | bash | fish>",
		Args:  cli.ArgsChoices(shell.Shells...),
		Flags: func(fs *cli.FlagSet) {
			cfg := fs.Config
			fs.Bool(&shOpts.noHooks, "no-hooks", !cfg.Shell.LogCommands, "leave out the command-logging hooks").ConfigKey("shell.log_commands")
			fs.Bool(&shOpts.noPrompt, "no-prompt", !cfg.Shell.PromptAuto, "leave out the plain prompt prefix").ConfigKey("shell.prompt_auto")
			fs.String(&shOpts.p10k, "p10k", cfg.Shell.P10K, "add the powerlevel10k segment").Placeholder("SIDE").
				Choices("left", "right", "none").ConfigKey("shell.p10k")
			fs.String(&shOpts.bin, "bin", "", "binary the functions call (default: this one)").Placeholder("PATH").Files()
			fs.Bool(&shOpts.write, "w,write", false, "also write it to <home>/shell/init.<shell>")
		},
		Examples: []cli.Example{
			{Cmd: "shell-init zsh", Desc: `use as: eval "$(px shell-init zsh)"`},
			{Cmd: "shell-init bash --no-hooks"},
			{Cmd: "shell-init fish | source"},
		},
		Run: runShellInit,
	})
	cli.Register(cli.Command{
		Name:    "prompt",
		Group:   cli.GroupShell,
		Summary: "print the prompt segment (for starship, oh-my-posh, tmux, ...)",
		Description: `Prints the current session's prompt text from the environment (nothing
when no proxy is on). Cheap: no database access. --format re-renders it
with the placeholders of shell.prompt_format: {icon} {scheme} {host} {port}
{ms} {cc} {id} {tags}.

  starship:  [custom.silkrouter]
             command = "silkrouter prompt"
             when = "test -n \"$SILKROUTER_ACTIVE\""
  tmux:      set -g status-right '#(silkrouter prompt --plain)'`,
		Flags: func(fs *cli.FlagSet) {
			fs.String(&shOpts.format, "f,format", "", "re-render with this format").Placeholder("FMT")
			fs.Bool(&shOpts.plain, "plain", false, "without the icon")
			fs.Bool(&shOpts.json, "json", false, "print the session variables as JSON")
		},
		Run: runPrompt,
	})
}

func sessionStore() shell.Store { return shell.Store{Dir: filepath.Join(paths.RunDir(), "sessions")} }

// emitShell is the shell to emit code for: --emit, or $SHELL with --print.
// "" means "called directly, not through the wrapper".
func emitShell() string {
	switch {
	case shOpts.emit != "":
		return shOpts.emit
	case shOpts.print:
		return shell.Detect()
	}
	return ""
}

func needWrapper(ctx *cli.Context, what string) int {
	ctx.Errorf("'%s %s' needs the shell integration (or --print).", ctx.Prog, what)
	fmt.Fprintln(ctx.Stderr)
	fmt.Fprintln(ctx.Stderr, shellIntro)
	return 1
}

// pickProxy chooses a proxy per s: -p, or the database pool (filters,
// tags, order) verified until one answers (unless NoVerify).
func pickProxy(ctx *cli.Context, d *db.DB, s *selOpts, avoid ...string) (proxy.Proxy, *db.Proxy, *probe.Result, error) {
	cfg := ctx.Config
	pr, err := probe.Get(s.Probe)
	if err != nil {
		return proxy.Proxy{}, nil, nil, err
	}
	timeout := s.Timeout.D()
	measure := func(p proxy.Proxy) probe.Result {
		r := pr.Measure(ctx, p, 1, timeout)
		if cfg.DB.Record {
			_, _ = d.RecordMeasurement(context.WithoutCancel(ctx), db.Measurement{Addr: db.AddrOf(p), Kind: db.KindPreflight,
				Probe: pr.Name, OK: r.OK, MS: r.LatencyMS, ErrClass: probe.ClassifyErr(r.Err), ExitIP: r.ExitIP})
		}
		return r
	}
	if s.Force != "" {
		p, err := proxy.Parse(s.Force)
		if err != nil {
			return proxy.Proxy{}, nil, nil, fmt.Errorf("bad -p proxy %q: %w", s.Force, err)
		}
		var res *probe.Result
		if !s.NoVerify {
			r := measure(p)
			if !r.OK {
				return proxy.Proxy{}, nil, nil, fmt.Errorf("%s does not answer: %v", p.Key(), r.Err)
			}
			res = &r
		}
		row, err := d.GetProxyByAddr(ctx, db.AddrOf(p))
		if err != nil {
			id, uerr := d.UpsertProxy(ctx, db.AddrOf(p), "session")
			if uerr != nil {
				return proxy.Proxy{}, nil, nil, uerr
			}
			row, _ = d.GetProxy(ctx, id)
		}
		return p, row, res, nil
	}
	f := db.Filter{Status: []string{db.StatusAlive}, MinMS: s.MinMS, MaxMS: s.MaxMS, IncludeUntimed: true,
		ExcludeTags: splitList(s.Exclude), WithTags: true}
	switch {
	case s.HTTP && !s.Socks:
		f.Schemes = db.ExpandSchemes([]string{"http"}, true)
	case s.Socks && !s.HTTP:
		f.Schemes = db.ExpandSchemes([]string{"socks"}, false)
	}
	rows, err := d.ListProxies(ctx, f)
	if err != nil {
		return proxy.Proxy{}, nil, nil, err
	}
	if tags := splitList(s.Tag); len(tags) > 0 {
		rows = withAnyTag(rows, tags)
	}
	if len(avoid) > 0 {
		skip := map[string]bool{}
		for _, a := range avoid {
			skip[a] = a != ""
		}
		kept := rows[:0:0]
		for _, r := range rows {
			if !skip[r.Key()] {
				kept = append(kept, r)
			}
		}
		// Everything excluded: better a known-bad upstream than none.
		if len(kept) > 0 {
			rows = kept
		}
	}
	prefer := s.Prefer
	if s.Best {
		prefer = "rank"
	}
	forCmd := s.For
	if forCmd == "" {
		forCmd = "shell"
	}
	rows = orderPool(rows, prefer, forCmd)
	if prefer == "random" {
		rand.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	}
	if len(rows) == 0 {
		return proxy.Proxy{}, nil, nil, errors.New("no proxies match (run 'px scan quick' or loosen the filters)")
	}
	if s.NoVerify {
		return rows[0].ToProxy(), rows[0], nil, nil
	}
	const maxTries = 25
	var lastErr error
	for i, row := range rows {
		if i >= maxTries || ctx.Err() != nil {
			break
		}
		r := measure(row.ToProxy())
		if r.OK {
			return row.ToProxy(), row, &r, nil
		}
		lastErr = r.Err
	}
	return proxy.Proxy{}, nil, nil, fmt.Errorf("none of %d candidates answered (last: %v)", min(len(rows), maxTries), lastErr)
}

func newSession(kind, sh string, p proxy.Proxy, row *db.Proxy, res *probe.Result, sel *selOpts) *shell.Session {
	s := &shell.Session{ID: shell.NewID(), Proxy: p.URL(), Key: p.Key(), Started: time.Now().UTC(), Shell: sh,
		PID: os.Getppid(), TTY: history.TTY(), Kind: kind}
	fillSession(s, p, row, res)
	s.Select, _ = json.Marshal(sel)
	return s
}

func fillSession(s *shell.Session, p proxy.Proxy, row *db.Proxy, res *probe.Result) {
	s.Proxy, s.Key = p.URL(), p.Key()
	s.MS, s.Country, s.Tags, s.ProxyID = 0, "", nil, 0
	if row != nil {
		s.ProxyID, s.Country, s.Tags, s.MS = row.ID, row.Country, row.Tags, row.LastMS
	}
	if res != nil && res.OK {
		s.MS = res.LatencyMS
	}
}

func onVars(ctx *cli.Context, s *shell.Session, p proxy.Proxy) []shell.Var {
	cfg := ctx.Config.Shell
	return shell.OnVars(s, cfg.NoProxy, cfg.Icon, cfg.Color, cfg.PromptFormat, cfg.LogCommands,
		string(p.Scheme), p.Host, p.Port, os.LookupEnv)
}

func announce(ctx *cli.Context, verb string, s *shell.Session) {
	c := ctx.ErrColors()
	var extra []string
	if s.MS > 0 {
		extra = append(extra, fmt.Sprintf("%dms", s.MS))
	}
	if s.Country != "" {
		extra = append(extra, s.Country)
	}
	if s.ProxyID > 0 {
		extra = append(extra, fmt.Sprintf("#%d", s.ProxyID))
	}
	info := ""
	if len(extra) > 0 {
		info = " (" + strings.Join(extra, ", ") + ")"
	}
	ctx.Infof("%s%s%s %s%s%s%s  session %s", c.Green, verb, c.Reset, c.Cyan, s.Key, c.Reset, info, s.ID)
}

func runOn(ctx *cli.Context, _ []string) int {
	sh := emitShell()
	if sh == "" && os.Getenv(shell.EnvShellFn) == "" {
		return needWrapper(ctx, "on")
	}
	if sh == "" {
		sh = shell.Detect()
	}
	if shOpts.relay {
		return runOnRelay(ctx, sh)
	}
	return withDB(ctx, func(d *db.DB) int {
		st := sessionStore()
		var s *shell.Session
		avoid := ""
		if id := os.Getenv(shell.EnvSession); id != "" && os.Getenv(shell.EnvActive) != "" {
			if old, err := st.Load(id); err == nil && old.Ended.IsZero() {
				s, avoid = old, old.Key
			}
		}
		p, row, res, err := pickProxy(ctx, d, &shOpts.sel, avoid)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if s == nil {
			s = newSession("on", sh, p, row, res, &shOpts.sel)
		} else {
			fillSession(s, p, row, res)
			s.Rotations++
			s.Select, _ = json.Marshal(&shOpts.sel)
		}
		if err := st.Save(s); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		fmt.Fprint(ctx.Stdout, shell.Emit(sh, onVars(ctx, s, p)))
		verb := "on"
		if s.Rotations > 0 {
			verb = "switched to"
		}
		announce(ctx, verb, s)
		return 0
	})
}

// runOnRelay points the shell at the local relay, starting it (with the
// given selection flags) when nothing answers at the address.
func runOnRelay(ctx *cli.Context, sh string) int {
	addr := shOpts.relayAddr
	st, ok := relayRunning(ctx, addr)
	if !ok {
		args := []string{"relay", "--listen", addr}
		args = append(args, selArgs(ctx, &shOpts.sel)...)
		if code := spawnRelay(ctx, addr, args); code != 0 {
			return code
		}
		if st, ok = relayRunning(ctx, addr); !ok {
			ctx.Errorf("relay at %s does not answer", addr)
			return 1
		}
	}
	socks, httpURL := relayURLs(addr)
	host, portS, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portS)
	s := &shell.Session{ID: shell.NewID(), Proxy: socks, HTTPProxy: httpURL, Key: "relay://" + addr, Started: time.Now().UTC(),
		Shell: sh, PID: os.Getppid(), TTY: history.TTY(), Kind: "on", Method: "relay"}
	if id := os.Getenv(shell.EnvSession); id != "" && os.Getenv(shell.EnvActive) != "" {
		if old, err := sessionStore().Load(id); err == nil && old.Ended.IsZero() {
			s.ID, s.Started, s.Rotations = old.ID, old.Started, old.Rotations+1
		}
	}
	if err := sessionStore().Save(s); err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	cfg := ctx.Config.Shell
	vars := shell.OnVars(s, cfg.NoProxy, cfg.Icon, cfg.Color, cfg.PromptFormat, cfg.LogCommands, "relay", host, port, os.LookupEnv)
	fmt.Fprint(ctx.Stdout, shell.Emit(sh, vars))
	ctx.Infof("on: relay %s (upstream %s, rotate %s)  session %s", addr, dash(st.Upstream), st.Rotate, s.ID)
	return 0
}

// selArgs turns the selection flags given on the command line back into
// arguments (to start a relay with the same selection).
func selArgs(ctx *cli.Context, s *selOpts) []string {
	var out []string
	add := func(name, flag, value string) {
		if ctx.Flags.Changed(name) {
			out = append(out, flag, value)
		}
	}
	if ctx.Flags.Changed("s") && s.Socks {
		out = append(out, "--socks")
	}
	if ctx.Flags.Changed("H") && s.HTTP {
		out = append(out, "--http")
	}
	add("d", "--max-ms", strconv.Itoa(s.MaxMS))
	add("D", "--min-ms", strconv.Itoa(s.MinMS))
	add("p", "--proxy", s.Force)
	add("T", "--tag", s.Tag)
	add("exclude-tag", "--exclude-tag", s.Exclude)
	add("for", "--for", s.For)
	add("prefer", "--prefer", s.Prefer)
	add("t", "--timeout", s.Timeout.String())
	add("probe", "--probe", s.Probe)
	if ctx.Flags.Changed("b") && s.Best {
		out = append(out, "--best")
	}
	if ctx.Flags.Changed("x") && s.NoVerify {
		out = append(out, "--no-verify")
	}
	return out
}

func runOff(ctx *cli.Context, _ []string) int {
	sh := emitShell()
	if sh == "" && os.Getenv(shell.EnvShellFn) == "" {
		return needWrapper(ctx, "off")
	}
	if sh == "" {
		sh = shell.Detect()
	}
	id := os.Getenv(shell.EnvSession)
	if id != "" {
		if s, err := sessionStore().End(id); err == nil && !ctx.Quiet {
			n := 0
			if d, err := openDB(ctx); err == nil {
				n, _ = d.CountUsage(ctx, db.UsageFilter{SessionID: id})
				d.Close()
			}
			ctx.Infof("off: session %s ended after %s, %d commands logged", id, roundDur(time.Since(s.Started)).Round(time.Second), n)
		}
	} else if os.Getenv(shell.EnvActive) == "" {
		ctx.Infof("no proxy is on in this shell")
	}
	fmt.Fprint(ctx.Stdout, shell.Emit(sh, shell.OffVars(os.LookupEnv)))
	return 0
}

func runRotate(ctx *cli.Context, _ []string) int {
	sh := emitShell()
	if sh == "" && os.Getenv(shell.EnvShellFn) == "" {
		return needWrapper(ctx, "rotate")
	}
	if sh == "" {
		sh = shell.Detect()
	}
	id := os.Getenv(shell.EnvSession)
	if id == "" {
		ctx.Errorf("no session in this shell: 'px on' first")
		return 1
	}
	st := sessionStore()
	s, err := st.Load(id)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	// Reuse the session's selection unless flags were given now.
	sel := shOpts.sel
	if len(s.Select) > 0 && !anyChanged(ctx, "s", "H", "d", "D", "p", "T", "exclude-tag", "b", "for", "prefer", "x", "t", "probe") {
		_ = json.Unmarshal(s.Select, &sel)
		if sel.Probe == "" {
			sel.Probe = shOpts.sel.Probe
		}
		if sel.Timeout == 0 {
			sel.Timeout = shOpts.sel.Timeout
		}
	}
	return withDB(ctx, func(d *db.DB) int {
		p, row, res, err := pickProxy(ctx, d, &sel, s.Key)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		fillSession(s, p, row, res)
		s.Rotations++
		if err := st.Save(s); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		fmt.Fprint(ctx.Stdout, shell.Emit(sh, onVars(ctx, s, p)))
		announce(ctx, "rotated to", s)
		return 0
	})
}

func anyChanged(ctx *cli.Context, names ...string) bool {
	for _, n := range names {
		if ctx.Flags.Changed(n) {
			return true
		}
	}
	return false
}

type sessionView struct {
	*shell.Session
	Active   bool `json:"active"`
	Commands int  `json:"commands"`
}

func runSessionStatus(ctx *cli.Context, _ []string) int {
	id := os.Getenv(shell.EnvSession)
	if id == "" || os.Getenv(shell.EnvActive) == "" {
		if shOpts.json {
			return ctx.JSON(map[string]any{"active": false})
		}
		ctx.Infof("no proxy is on in this shell ('px on' or 'px shell')")
		return 1
	}
	return showSession(ctx, id)
}

func showSession(ctx *cli.Context, id string) int {
	s, err := sessionStore().Load(id)
	if err != nil {
		ctx.Errorf("session %s: %v", id, err)
		return 1
	}
	v := sessionView{Session: s, Active: s.Active()}
	var recent []db.Usage
	if d, err := openDB(ctx); err == nil {
		v.Commands, _ = d.CountUsage(ctx, db.UsageFilter{SessionID: id})
		recent, _ = d.ListUsage(ctx, db.UsageFilter{SessionID: id, Limit: 10})
		d.Close()
	}
	if shOpts.json {
		return ctx.JSON(v)
	}
	c := ctx.Colors()
	state := c.Green + "active" + c.Reset
	if !v.Active {
		state = c.Gray + "ended" + c.Reset
	}
	kv := func(k, val string) { fmt.Fprintf(ctx.Stdout, "  %s%-10s%s %s\n", c.Cyan, k, c.Reset, val) }
	fmt.Fprintf(ctx.Stdout, "%ssession %s%s  %s  %s\n", c.Bold, s.ID, c.Reset, state, s.Kind)
	kv("proxy", s.Key)
	if s.ProxyID > 0 {
		kv("id", fmt.Sprintf("#%d", s.ProxyID))
	}
	kv("latency", msOrDash(s.MS))
	kv("country", dash(s.Country))
	if len(s.Tags) > 0 {
		kv("tags", c.Magenta+strings.Join(s.Tags, " ")+c.Reset)
	}
	kv("started", fmtTime(s.Started))
	if !s.Ended.IsZero() {
		kv("ended", fmtTime(s.Ended))
	}
	kv("shell", fmt.Sprintf("%s pid %d %s", dash(s.Shell), s.PID, s.TTY))
	kv("rotations", strconv.Itoa(s.Rotations))
	kv("commands", strconv.Itoa(v.Commands))
	if len(recent) > 0 {
		fmt.Fprintln(ctx.Stdout)
		printUsageTable(ctx, recent, false)
	}
	return 0
}

func runSessionList(ctx *cli.Context, _ []string) int {
	all, err := sessionStore().List()
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	var views []sessionView
	d, _ := openDB(ctx)
	for _, s := range all {
		v := sessionView{Session: s, Active: s.Active()}
		if !v.Active && !shOpts.all {
			continue
		}
		if d != nil {
			v.Commands, _ = d.CountUsage(ctx, db.UsageFilter{SessionID: s.ID})
		}
		views = append(views, v)
	}
	if d != nil {
		d.Close()
	}
	if shOpts.json {
		if views == nil {
			views = []sessionView{}
		}
		return ctx.JSON(views)
	}
	c := ctx.Colors()
	if len(views) == 0 {
		fmt.Fprintf(ctx.Stdout, "%sno sessions (-a shows ended ones)%s\n", c.Gray, c.Reset)
		return 0
	}
	fmt.Fprintf(ctx.Stdout, "%s%-12s  %-8s  %-8s  %-28s  %6s  %8s  %s%s\n", c.Bold, "SESSION", "KIND", "STATE", "PROXY", "CMDS", "AGE", "TTY", c.Reset)
	for _, v := range views {
		state := c.Green + "active  " + c.Reset
		if !v.Active {
			state = c.Gray + "ended   " + c.Reset
		}
		fmt.Fprintf(ctx.Stdout, "%-12s  %-8s  %s  %s%-28s%s  %6d  %8s  %s\n", v.ID, v.Kind, state, c.Cyan, v.Key, c.Reset,
			v.Commands, ago(v.Started), dash(v.TTY))
	}
	return 0
}

func runSessionShow(ctx *cli.Context, args []string) int {
	if len(args) != 1 {
		return ctx.UsageError("give one session id")
	}
	return showSession(ctx, args[0])
}

func runSessionEnd(ctx *cli.Context, args []string) int {
	if len(args) == 0 {
		return ctx.UsageError("give session ids")
	}
	for _, id := range args {
		if _, err := sessionStore().End(id); err != nil {
			ctx.Errorf("%s: %v", id, err)
			return 1
		}
	}
	return 0
}

func runSessionPrune(ctx *cli.Context, _ []string) int {
	before, err := history.ParseSince(shOpts.olderThan, time.Now())
	if err != nil {
		return ctx.UsageError("%v", err)
	}
	n, err := sessionStore().Prune(before)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	ctx.Infof("deleted %d session files", n)
	return 0
}

// ---- px shell (subshell) -----------------------------------------------------------

func initOptions(ctx *cli.Context) shell.InitOptions {
	bin := shOpts.bin
	if bin == "" {
		if exe, err := os.Executable(); err == nil {
			bin = exe
		} else {
			bin = "silkrouter"
		}
	}
	cfg := ctx.Config.Shell
	hooks, prompt, p10k := cfg.LogCommands, cfg.PromptAuto, cfg.P10K
	if ctx.Command != nil && ctx.Command.Name == "shell-init" {
		hooks, prompt, p10k = !shOpts.noHooks, !shOpts.noPrompt, shOpts.p10k
	}
	return shell.InitOptions{Bin: bin, Hooks: hooks, P10K: p10k, PromptAuto: prompt, Version: version}
}

// writeSnippet writes <home>/shell/init.<sh> and returns its path.
func writeSnippet(ctx *cli.Context, sh string) (string, error) {
	script, err := shell.Init(sh, initOptions(ctx))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(paths.ShellDir(), 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(paths.ShellDir(), "init."+sh)
	return path, os.WriteFile(path, []byte(script), 0o644)
}

func runShellInit(ctx *cli.Context, args []string) int {
	if len(args) != 1 {
		return ctx.UsageError("need a shell: zsh, bash or fish")
	}
	script, err := shell.Init(args[0], initOptions(ctx))
	if err != nil {
		return ctx.UsageError("%v", err)
	}
	if shOpts.write {
		if err := paths.Ensure(); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		path, err := writeSnippet(ctx, args[0])
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		ctx.Infof("wrote %s", paths.Pretty(path))
	}
	fmt.Fprint(ctx.Stdout, script)
	return 0
}

func runSubshell(ctx *cli.Context, args []string) int {
	if len(args) > 0 {
		return ctx.UsageError("px shell takes no arguments (use 'px <command>' to route one command)")
	}
	prog := shOpts.shell
	if prog == "" {
		prog = os.Getenv("SHELL")
	}
	if prog == "" {
		ctx.Errorf("no shell: set $SHELL or use --shell")
		return 1
	}
	path, err := exec.LookPath(prog)
	if err != nil {
		ctx.Errorf("%s: %v", prog, err)
		return 127
	}
	sh := strings.TrimSuffix(filepath.Base(path), ".exe")
	method := shOpts.method
	if shOpts.chain {
		method = router.MethodProxychains
	}
	if method == router.MethodProxychains {
		if _, err := router.FindProxychains(ctx.Config.Route.Proxychains); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
	}
	d, err := openDB(ctx)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	p, row, res, err := pickProxy(ctx, d, &shOpts.sel, "")
	d.Close()
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	s := newSession("subshell", sh, p, row, res, &shOpts.sel)
	s.Method = method
	s.PID = 0
	st := sessionStore()
	if err := st.Save(s); err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	vars := onVars(ctx, s, p)
	vars = append(vars, shell.Var{Name: shell.EnvSubshell, Value: "1"})
	env := shell.EnvList(os.Environ(), vars)
	argv := []string{path}
	var cleanup []func()
	if shell.Supported(sh) {
		snippet, err := writeSnippet(ctx, sh)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		dir := filepath.Join(paths.RunDir(), "shell-"+s.ID)
		extra, extraEnv, err := shell.Bootstrap(sh, dir, snippet)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		cleanup = append(cleanup, func() { os.RemoveAll(dir) })
		argv = append(argv, extra...)
		env = append(env, extraEnv...)
	}
	if method == router.MethodProxychains {
		wrapped, done, err := router.ProxychainsWrap(ctx.Config.Route.Proxychains, paths.RunDir())(p, argv)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		argv = wrapped
		cleanup = append(cleanup, done)
	}
	defer func() {
		for _, f := range cleanup {
			f()
		}
	}()
	announce(ctx, "subshell on", s)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, os.Stdout, os.Stderr, env
	code := 0
	if err := cmd.Start(); err != nil {
		ctx.Errorf("%v", err)
		return 127
	}
	s.PID = cmd.Process.Pid
	_ = st.Save(s)
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = 1
		}
	}
	if ended, err := st.End(s.ID); err == nil {
		ctx.Infof("subshell session %s ended after %s", s.ID, roundDur(time.Since(ended.Started)).Round(time.Second))
	}
	return code
}

// ---- install / uninstall / prompt ------------------------------------------------------

func targetShells(ctx *cli.Context, args []string) ([]string, error) {
	shells := args
	if len(shells) == 0 {
		shells = ctx.Config.Shell.Shells
	}
	if len(shells) == 0 {
		shells = []string{shell.Detect()}
	}
	for _, sh := range shells {
		if !shell.Supported(sh) {
			return nil, fmt.Errorf("unsupported shell %q (want %s)", sh, strings.Join(shell.Shells, ", "))
		}
	}
	if shOpts.rc != "" && len(shells) != 1 {
		return nil, fmt.Errorf("--rc needs exactly one shell")
	}
	return shells, nil
}

func rcFor(sh string) (string, error) {
	if shOpts.rc != "" {
		return paths.ExpandUser(shOpts.rc), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return shell.RCFile(sh, home), nil
}

func runShellInstall(ctx *cli.Context, args []string) int {
	shells, err := targetShells(ctx, args)
	if err != nil {
		return ctx.UsageError("%v", err)
	}
	c := ctx.Colors()
	for _, sh := range shells {
		rc, err := rcFor(sh)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		snippet := filepath.Join(paths.ShellDir(), "init."+sh)
		if !shOpts.dryRun {
			if snippet, err = writeSnippet(ctx, sh); err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
		}
		block := shell.Block(sh, snippet, "silkrouter")
		res, err := shell.Install(rc, block, shOpts.noBackup, shOpts.dryRun)
		if err != nil {
			ctx.Errorf("%s: %v", paths.Pretty(rc), err)
			return 1
		}
		switch {
		case shOpts.dryRun:
			fmt.Fprintf(ctx.Stdout, "%s%s%s: would %s this block:\n%s", c.Bold, paths.Pretty(rc), c.Reset,
				map[bool]string{true: "refresh", false: "append"}[res.Existed], block)
		case !res.Changed:
			ctx.Infof("%s: already up to date", paths.Pretty(rc))
		default:
			msg := fmt.Sprintf("%s: integration %s", paths.Pretty(rc), map[bool]string{true: "refreshed", false: "installed"}[res.Existed])
			if res.Backup != "" {
				msg += " (backup " + paths.Pretty(res.Backup) + ")"
			}
			ctx.Infof("%s", msg)
		}
	}
	if !shOpts.dryRun {
		ctx.Infof("open a new shell (or source the rc file), then: px on")
	}
	return 0
}

func runShellUninstall(ctx *cli.Context, args []string) int {
	shells, err := targetShells(ctx, args)
	if err != nil {
		return ctx.UsageError("%v", err)
	}
	for _, sh := range shells {
		rc, err := rcFor(sh)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		res, err := shell.Uninstall(rc, shOpts.noBackup, shOpts.dryRun)
		if err != nil {
			ctx.Errorf("%s: %v", paths.Pretty(rc), err)
			return 1
		}
		switch {
		case !res.Existed:
			ctx.Infof("%s: no integration block", paths.Pretty(rc))
		case shOpts.dryRun:
			ctx.Infof("%s: would remove the integration block", paths.Pretty(rc))
		default:
			ctx.Infof("%s: integration removed (backup %s)", paths.Pretty(rc), dash(paths.Pretty(res.Backup)))
		}
	}
	return 0
}

func runPrompt(ctx *cli.Context, _ []string) int {
	if os.Getenv(shell.EnvActive) == "" {
		if shOpts.json {
			return ctx.JSON(map[string]any{"active": false})
		}
		return 0
	}
	if shOpts.json {
		out := map[string]any{"active": true}
		for _, k := range []string{shell.EnvProxyKey, shell.EnvProxyID, shell.EnvProxyMS, shell.EnvProxyCC, shell.EnvSession,
			shell.EnvSince, shell.EnvPrompt, shell.EnvMethod} {
			out[strings.ToLower(strings.TrimPrefix(k, "SILKROUTER_"))] = os.Getenv(k)
		}
		return ctx.JSON(out)
	}
	text := os.Getenv(shell.EnvPrompt)
	if shOpts.plain {
		text = os.Getenv(shell.EnvPromptText)
	}
	if shOpts.format != "" {
		f := shell.PromptFields{Icon: os.Getenv(shell.EnvIcon), Country: os.Getenv(shell.EnvProxyCC)}
		if shOpts.plain {
			f.Icon = ""
		}
		if p, err := proxy.Parse(os.Getenv(shell.EnvProxyKey)); err == nil {
			f.Scheme, f.Host, f.Port = string(p.Scheme), p.Host, p.Port
		}
		f.MS, _ = strconv.Atoi(os.Getenv(shell.EnvProxyMS))
		id, _ := strconv.ParseInt(os.Getenv(shell.EnvProxyID), 10, 64)
		f.ID = id
		text = shell.RenderPrompt(shOpts.format, f)
	}
	fmt.Fprintln(ctx.Stdout, text)
	return 0
}
