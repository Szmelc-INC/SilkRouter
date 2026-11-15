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
	"strings"
	"sync"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/history"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/relay"
	"github.com/szmelc-inc/silkrouter/internal/shell"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

var relayOpts struct {
	sel         selOpts
	listen      string
	rotate      string
	interval    config.Duration
	maxTries    int
	dialTimeout config.Duration
	auth        string
	daemon      bool
	noLog       bool
	json        bool
	shell       string
}

// envRelayDaemon marks the detached child started by --daemon.
const envRelayDaemon = "SILKROUTER_RELAY_DAEMON"

func relayAddrFlag(fs *cli.FlagSet) {
	fs.String(&relayOpts.listen, "l,listen", fs.Config.Relay.Listen, "relay address").Placeholder("ADDR").ConfigKey("relay.listen")
}

func init() {
	cli.Register(cli.Command{
		Name:    "relay",
		Group:   cli.GroupShell,
		Summary: "local proxy on 127.0.0.1 over rotating upstreams (SOCKS5 / SOCKS4 / HTTP, failover)",
		Description: `Runs a local proxy (relay.listen, default 127.0.0.1:18080) that speaks
SOCKS5, SOCKS4/4a and HTTP (CONNECT and plain requests) on one port and
forwards every connection through an upstream proxy from the database,
picked like 'px on' (filters, tags, --best, verified first unless -x).

Failover: when the upstream cannot open a tunnel, the next one is tried
(relay.max_tries per connection) and becomes the current one. Rotation
(relay.rotate): sticky keeps an upstream until it fails, per-conn uses a new
one for every connection, interval switches every relay.interval.

Point anything at it once and keep the address while upstreams change:
'px relay env' prints the variables, 'px on --relay' exports them in your
shell (and starts the relay when needed). --daemon detaches it (log in
<home>/logs/relay.log); 'px relay status | rotate | stop' control it. Every
upstream stint is a usage row (mode relay) and tunnel results are recorded
as measurements (relay.record).`,
		Usage:     "[selection options] [relay options]",
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			cfg := fs.Config
			selFlags(fs, &relayOpts.sel)
			relayAddrFlag(fs)
			fs.String(&relayOpts.rotate, "rotate", cfg.Relay.Rotate, "upstream rotation").Placeholder("MODE").
				Choices(relay.RotateModes...).ConfigKey("relay.rotate")
			fs.Duration(&relayOpts.interval, "every", cfg.Relay.Interval, "rotation period for --rotate interval").
				ConfigKey("relay.interval")
			fs.Int(&relayOpts.maxTries, "max-tries", cfg.Relay.MaxTries, "upstreams tried per connection").ConfigKey("relay.max_tries")
			fs.Duration(&relayOpts.dialTimeout, "dial-timeout", cfg.Relay.DialTimeout, "tunnel setup timeout per upstream").
				ConfigKey("relay.dial_timeout")
			fs.String(&relayOpts.auth, "auth", cfg.Relay.Auth, "require USER:PASS from clients").Placeholder("USER:PASS").
				ConfigKey("relay.auth").NoDefault()
			fs.Bool(&relayOpts.daemon, "daemon", false, "run in the background (log: <home>/logs/relay.log)")
			fs.Bool(&relayOpts.noLog, "no-log", !cfg.Relay.Record, "do not record usage / measurements").ConfigKey("relay.record")
		},
		Examples: []cli.Example{
			{Cmd: "relay", Desc: "foreground, Ctrl-C stops it"},
			{Cmd: "relay --daemon -s --best", Desc: "background, best socks upstreams"},
			{Cmd: "relay --rotate interval --every 5m --tag elite"},
			{Cmd: "relay env", Desc: `then: eval "$(px relay env)"`},
			{Cmd: "relay rotate"},
		},
		Run: runRelay,
		Subcommands: []cli.Command{
			{
				Name: "status", Summary: "show the running relay (upstream, connections, traffic)",
				Flags: func(fs *cli.FlagSet) {
					relayAddrFlag(fs)
					fs.Bool(&relayOpts.json, "json", false, "print as JSON")
				},
				Run: runRelayStatus,
			},
			{
				Name: "rotate", Summary: "switch the running relay to another upstream now",
				Flags: relayAddrFlag,
				Run:   func(ctx *cli.Context, _ []string) int { return relayControl(ctx, "rotate") },
			},
			{
				Name: "stop", Summary: "stop the running relay",
				Flags: relayAddrFlag,
				Run:   func(ctx *cli.Context, _ []string) int { return relayControl(ctx, "stop") },
			},
			{
				Name: "env", Summary: "print the proxy variables pointing at the relay",
				Usage: "[--shell zsh|bash|fish]",
				Flags: func(fs *cli.FlagSet) {
					relayAddrFlag(fs)
					fs.String(&relayOpts.shell, "shell", "", "syntax (default: $SHELL)").Placeholder("SHELL").Choices(shell.Shells...)
				},
				Run: runRelayEnv,
			},
		},
	})
}

// relayState is <home>/run/relay.json while a relay runs.
type relayState struct {
	PID     int       `json:"pid"`
	Listen  string    `json:"listen"`
	Started time.Time `json:"started"`
}

func relayStatePath() string { return filepath.Join(paths.RunDir(), "relay.json") }

// relayURLs are the ALL_PROXY (socks5h, remote DNS) and HTTP(S)_PROXY
// URLs of a relay address.
func relayURLs(addr string) (socks, httpURL string) {
	return "socks5h://" + addr, "http://" + addr
}

// relayRunning asks addr for its status.
func relayRunning(ctx context.Context, addr string) (relay.Status, bool) {
	var st relay.Status
	err := relay.Control(ctx, addr, "status", &st)
	return st, err == nil
}

func runRelay(ctx *cli.Context, args []string) int {
	if len(args) > 0 {
		return ctx.UsageError("unexpected arguments: %s", strings.Join(args, " "))
	}
	cfg := ctx.Config
	addr := relayOpts.listen
	if st, ok := relayRunning(ctx, addr); ok {
		ctx.Errorf("a relay already runs at %s (upstream %s); 'px relay stop' first", addr, dash(st.Upstream))
		return 1
	}
	if relayOpts.daemon {
		return startRelayDaemon(ctx, addr)
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && relayOpts.auth == "" {
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			ctx.Infof("warning: %s is not a loopback address and no --auth is set: anyone who can reach it can use your upstreams", addr)
		}
	}
	d, err := openDB(ctx)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	defer d.Close()
	sel := relayOpts.sel
	if sel.For == "" {
		sel.For = "relay"
	}
	rec := newRelayRecorder(ctx, d, addr, cfg.Relay.Record && !relayOpts.noLog)
	defer rec.close()
	pool := &relayPool{ctx: ctx, d: d, sel: sel, perConn: relayOpts.rotate == relay.RotatePerConn}
	srv := relay.New(relay.Options{
		Pick: pool.pick, Rotate: relayOpts.rotate, Interval: relayOpts.interval.D(), MaxTries: relayOpts.maxTries,
		DialTimeout: relayOpts.dialTimeout.D(), Auth: relayOpts.auth, OnEvent: rec.event,
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	// Pick the first upstream now so a broken selection fails fast.
	if relayOpts.rotate != relay.RotatePerConn {
		if _, err := srv.Rotate(ctx, "start"); err != nil {
			ln.Close()
			ctx.Errorf("%v", err)
			return 1
		}
	}
	state := relayState{PID: os.Getpid(), Listen: ln.Addr().String(), Started: time.Now().UTC()}
	if data, err := json.MarshalIndent(state, "", "  "); err == nil {
		_ = os.WriteFile(relayStatePath(), data, 0o600)
	}
	defer os.Remove(relayStatePath())
	socks, httpURL := relayURLs(ln.Addr().String())
	ctx.Infof("relay on %s (socks5 / socks4 / http), rotate %s; use ALL_PROXY=%s HTTP(S)_PROXY=%s", ln.Addr(), relayOpts.rotate, socks, httpURL)
	if cur, ok := srv.Current(); ok {
		ctx.Infof("upstream %s", cur.Key())
	}
	stop := relayStatusLine(ctx, srv)
	err = srv.Serve(ctx, ln)
	stop()
	st := srv.Status()
	ctx.Infof("relay stopped: %d connections, %d failed tunnels, %d switches, %s up / %s down",
		st.Conns, st.Fails, st.Switches, humanBytes(st.BytesUp), humanBytes(st.BytesDown))
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	return 0
}

// relayStatusLine refreshes a one-line status on a terminal (foreground).
func relayStatusLine(ctx *cli.Context, srv *relay.Server) func() {
	f, ok := ctx.Stderr.(*os.File)
	if ctx.Quiet || !ok || !ui.IsTerminal(f) || os.Getenv(envRelayDaemon) != "" {
		return func() {}
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		c := ctx.ErrColors()
		for {
			select {
			case <-quit:
				fmt.Fprint(ctx.Stderr, "\r\033[2K")
				return
			case <-t.C:
				st := srv.Status()
				fmt.Fprintf(ctx.Stderr, "\r\033[2K%s⇄%s %s%s%s  %d conns (%d active)  %s%d fails%s  %d switches  %s↑ %s↓",
					c.Magenta, c.Reset, c.Cyan, dash(st.Upstream), c.Reset, st.Conns, st.Active, c.Red, st.Fails, c.Reset,
					st.Switches, humanBytes(st.BytesUp), humanBytes(st.BytesDown))
			}
		}
	}()
	return func() { close(quit); <-done }
}

// relayPool picks upstreams from the database for the relay.
type relayPool struct {
	ctx     *cli.Context
	d       *db.DB
	sel     selOpts
	perConn bool

	mu   sync.Mutex
	rows []*db.Proxy
	at   time.Time
}

func (rp *relayPool) pick(ctx context.Context, avoid []string) (proxy.Proxy, error) {
	if !rp.perConn {
		// sticky / interval: verified pick (rare: start, failover, rotation).
		p, _, _, err := pickProxy(rp.ctx, rp.d, &rp.sel, avoid...)
		return p, err
	}
	// per-conn: round-robin over a cached, ordered pool; failover handles
	// the dead ones, so no verification on every connection.
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.rows == nil || time.Since(rp.at) > 5*time.Minute {
		sel := rp.sel
		f := db.Filter{Status: []string{db.StatusAlive}, MinMS: sel.MinMS, MaxMS: sel.MaxMS, IncludeUntimed: true,
			ExcludeTags: splitList(sel.Exclude), WithTags: true}
		switch {
		case sel.HTTP && !sel.Socks:
			f.Schemes = db.ExpandSchemes([]string{"http"}, true)
		case sel.Socks && !sel.HTTP:
			f.Schemes = db.ExpandSchemes([]string{"socks"}, false)
		}
		rows, err := rp.d.ListProxies(ctx, f)
		if err != nil {
			return proxy.Proxy{}, err
		}
		if tags := splitList(sel.Tag); len(tags) > 0 {
			rows = withAnyTag(rows, tags)
		}
		prefer := sel.Prefer
		if sel.Best {
			prefer = "rank"
		}
		rows = orderPool(rows, prefer, sel.For)
		if prefer == "random" {
			rand.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
		}
		rp.rows, rp.at = rows, time.Now()
	}
	if len(rp.rows) == 0 {
		return proxy.Proxy{}, errors.New("no proxies match (run 'px scan quick' or loosen the filters)")
	}
	skip := map[string]bool{}
	for _, a := range avoid {
		skip[a] = true
	}
	for i := 0; i < len(rp.rows); i++ {
		p := rp.rows[0]
		rp.rows = append(rp.rows[1:], p) // rotate the ring
		if !skip[p.Key()] {
			return p.ToProxy(), nil
		}
	}
	return rp.rows[0].ToProxy(), nil
}

// relayRecorder writes relay events into the database: failed tunnels
// always, successful ones sampled (relay.sample_every per upstream), and
// one usage row (mode relay) per upstream stint.
type relayRecorder struct {
	ctx    *cli.Context
	d      *db.DB
	w      *db.Writer
	addr   string
	on     bool
	every  time.Duration
	mu     sync.Mutex
	last   map[string]time.Time
	stint  int64
	start  time.Time
	failed string // err_class of the failure that ended the stint
}

func newRelayRecorder(ctx *cli.Context, d *db.DB, addr string, on bool) *relayRecorder {
	r := &relayRecorder{ctx: ctx, d: d, addr: addr, on: on, every: ctx.Config.Relay.SampleEvery.D(), last: map[string]time.Time{}}
	if on {
		r.w = d.NewWriter(context.WithoutCancel(ctx), db.WriterOptions{BatchSize: 100, FlushInterval: 2 * time.Second})
	}
	return r
}

func (r *relayRecorder) event(e relay.Event) {
	if !r.on {
		return
	}
	bg := context.WithoutCancel(r.ctx)
	switch e.Kind {
	case relay.EventDialFail:
		class := probe.ClassifyErr(e.Err)
		_ = r.w.Measure(db.Measurement{Addr: db.AddrOf(e.Proxy), Kind: db.KindRoute, Probe: "relay", OK: false,
			ErrClass: class, Detail: `{"target":` + quoteJSON(e.Target) + `}`})
		r.mu.Lock()
		r.failed = class
		r.mu.Unlock()
	case relay.EventDialOK:
		r.mu.Lock()
		due := time.Since(r.last[e.Proxy.Key()]) >= r.every
		if due {
			r.last[e.Proxy.Key()] = time.Now()
		}
		r.mu.Unlock()
		if due {
			_ = r.w.Measure(db.Measurement{Addr: db.AddrOf(e.Proxy), Kind: db.KindRoute, Probe: "relay", OK: true, MS: e.MS,
				Detail: `{"target":` + quoteJSON(e.Target) + `}`})
		}
	case relay.EventSwitch:
		r.mu.Lock()
		defer r.mu.Unlock()
		r.finishLocked(bg)
		var id int64
		if row, err := r.d.GetProxyByAddr(bg, db.AddrOf(e.Proxy)); err == nil {
			id = row.ID
		}
		cwd, _ := os.Getwd()
		uid, err := r.d.StartUsage(bg, db.Usage{ProxyID: id, ProxyURL: e.Proxy.Key(), Mode: db.ModeRelay,
			Argv: []string{"relay", r.addr, "(" + e.Reason + ")"}, Cmd: "relay", Cwd: cwd, TTY: history.TTY(),
			PID: os.Getpid(), PPID: os.Getppid(), SessionID: os.Getenv(shell.EnvSession)})
		if err == nil {
			r.stint, r.start = uid, time.Now()
		}
	}
}

func (r *relayRecorder) finishLocked(ctx context.Context) {
	if r.stint == 0 {
		return
	}
	code := 0
	if r.failed != "" {
		code = 1
	}
	_ = r.d.FinishUsageWith(ctx, r.stint, db.UsageEnd{ExitCode: code, ErrClass: r.failed})
	r.stint, r.failed = 0, ""
}

func (r *relayRecorder) close() {
	if !r.on {
		return
	}
	r.mu.Lock()
	r.failed = "" // stopping is not a failure
	r.finishLocked(context.WithoutCancel(r.ctx))
	r.mu.Unlock()
	_ = r.w.Close()
}

// startRelayDaemon re-runs this command (px relay ... without --daemon)
// detached, logging to <home>/logs/relay.log, and waits until it answers.
func startRelayDaemon(ctx *cli.Context, addr string) int {
	var args []string
	for _, a := range os.Args[1:] {
		if a != "--daemon" && a != "-daemon" && a != "--daemon=true" && a != "-daemon=true" {
			args = append(args, a)
		}
	}
	return spawnRelay(ctx, addr, args)
}

// spawnRelay starts `silkrouter <args>` (args must run the relay) detached
// and waits until addr answers.
func spawnRelay(ctx *cli.Context, addr string, args []string) int {
	exe, err := os.Executable()
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	if !containsString(args, "relay") {
		args = append([]string{"relay", "--listen", addr}, args...)
	}
	if err := paths.Ensure(); err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	logPath := filepath.Join(paths.LogsDir(), "relay.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	defer logf.Close()
	fmt.Fprintf(logf, "---- %s relay start %s\n", time.Now().Format(time.RFC3339), strings.Join(args, " "))
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), envRelayDaemon+"=1", "NO_COLOR=1")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := relayRunning(ctx, addr); ok {
			ctx.Infof("relay running at %s (pid %d, upstream %s); log %s", addr, pid, dash(st.Upstream), paths.Pretty(logPath))
			return 0
		}
		time.Sleep(200 * time.Millisecond)
	}
	ctx.Errorf("the relay did not come up within 15s; see %s", paths.Pretty(logPath))
	return 1
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func runRelayStatus(ctx *cli.Context, _ []string) int {
	st, ok := relayRunning(ctx, relayOpts.listen)
	if !ok {
		if relayOpts.json {
			return ctx.JSON(map[string]any{"running": false, "listen": relayOpts.listen})
		}
		ctx.Infof("no relay at %s ('px relay --daemon' starts one)", relayOpts.listen)
		return 1
	}
	if relayOpts.json {
		return ctx.JSON(st)
	}
	c := ctx.Colors()
	kv := func(k, v string) { fmt.Fprintf(ctx.Stdout, "  %s%-10s%s %s\n", c.Cyan, k, c.Reset, v) }
	fmt.Fprintf(ctx.Stdout, "%srelay %s%s  %srunning%s for %s\n", c.Bold, st.Listen, c.Reset, c.Green, c.Reset,
		roundDur(time.Since(st.Started)).Round(time.Second))
	kv("upstream", dash(st.Upstream)+"  "+c.Gray+"since "+ago(st.Since)+c.Reset)
	kv("rotate", st.Rotate)
	kv("conns", fmt.Sprintf("%d (%d active)", st.Conns, st.Active))
	kv("failed", fmt.Sprintf("%d tunnels, %d switches", st.Fails, st.Switches))
	kv("traffic", fmt.Sprintf("%s up, %s down", humanBytes(st.BytesUp), humanBytes(st.BytesDown)))
	socks, httpURL := relayURLs(st.Listen)
	kv("use", "ALL_PROXY="+socks+"  HTTP(S)_PROXY="+httpURL)
	return 0
}

func relayControl(ctx *cli.Context, endpoint string) int {
	var out map[string]any
	if err := relay.Control(ctx, relayOpts.listen, endpoint, &out); err != nil {
		ctx.Errorf("relay at %s: %v", relayOpts.listen, err)
		return 1
	}
	switch endpoint {
	case "rotate":
		ctx.Infof("relay switched to %v", out["upstream"])
	case "stop":
		ctx.Infof("relay at %s stopping", relayOpts.listen)
	}
	return 0
}

func runRelayEnv(ctx *cli.Context, _ []string) int {
	sh := relayOpts.shell
	if sh == "" {
		sh = shell.Detect()
	}
	socks, httpURL := relayURLs(relayOpts.listen)
	var vars []shell.Var
	for _, k := range shell.ProxyVars {
		v := httpURL
		if strings.EqualFold(k, "ALL_PROXY") {
			v = socks
		}
		vars = append(vars, shell.Var{Name: k, Value: v})
	}
	for _, k := range shell.NoProxyVars {
		vars = append(vars, shell.Var{Name: k, Value: ctx.Config.Shell.NoProxy})
	}
	fmt.Fprint(ctx.Stdout, shell.Emit(sh, vars))
	if _, ok := relayRunning(ctx, relayOpts.listen); !ok {
		ctx.Infof("note: no relay is running at %s yet ('px relay --daemon')", relayOpts.listen)
	}
	return 0
}
