package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/history"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

type histOptions struct {
	proxy   string
	cmd     string
	session string
	mode    string
	since   string
	until   string
	status  string
	search  string
	limit   int
	offset  int
	all     bool
	full    bool
	json    bool
	by      string
	sort    string
	format  string
	output  string
	older   string
	yes     bool
	logPath bool
	// __shell-log
	shProxy string
	shSess  string
	shStart int64
	shEnd   int64
	shExit  int
	shCwd   string
	shTTY   string
}

var histOpts histOptions

func histFilterFlags(fs *cli.FlagSet, paging bool) {
	o := &histOpts
	fs.String(&o.proxy, "p,proxy", "", "only this proxy (id, URL or host[:port])").Placeholder("PROXY")
	fs.String(&o.cmd, "c,cmd", "", "only this command (basename, e.g. curl)").Placeholder("CMD").Dynamic("used-commands")
	fs.String(&o.session, "session", "", "only this shell session id").Placeholder("ID")
	fs.String(&o.mode, "mode", "", "only this mode").Placeholder("MODE").Choices(db.Modes...)
	fs.String(&o.since, "s,since", "", "only since this time (30m, 12h, 7d, 2w, 2026-11-01)").Placeholder("WHEN")
	fs.String(&o.until, "until", "", "only before this time").Placeholder("WHEN")
	fs.String(&o.status, "status", "", "ok, fail, proxy (network / proxy errors) or an err_class").Placeholder("STATUS").
		Choices(append([]string{"ok", "fail", "proxy"}, db.ErrClasses...)...)
	fs.String(&o.search, "S,search", "", "substring of the command line, cwd or proxy").Placeholder("TEXT")
	if paging {
		fs.Int(&o.limit, "n,limit", fs.Config.History.ListLimit, "show at most N rows (0 = all)").ConfigKey("history.list_limit")
		fs.Int(&o.offset, "offset", 0, "skip the first N rows")
		fs.Bool(&o.all, "a,all", false, "no row limit")
	}
}

func (o *histOptions) build(ctx *cli.Context, d *db.DB, words []string) (db.UsageFilter, error) {
	now := time.Now()
	f := db.UsageFilter{Cmd: o.cmd, SessionID: o.session, Mode: o.mode, ErrClass: o.status,
		Search: strings.TrimSpace(o.search + " " + strings.Join(words, " ")), Limit: o.limit, Offset: o.offset}
	if o.all {
		f.Limit = 0
	}
	var err error
	if f.Since, err = history.ParseSince(o.since, now); err != nil {
		return f, err
	}
	if f.Until, err = history.ParseSince(o.until, now); err != nil {
		return f, err
	}
	if o.proxy != "" {
		p, err := d.FindProxy(ctx, o.proxy)
		if err != nil {
			return f, err
		}
		f.ProxyID = p.ID
	}
	return f, nil
}

func init() {
	cli.Register(cli.Command{
		Name:    "history",
		Aliases: []string{"hist", "log"},
		Group:   cli.GroupHistory,
		Summary: "the usage log: every proxied command (when, proxy, command, result)",
		Description: `Lists the usage log, newest first: every command run through a proxy by
route (px <cmd>), the shell hooks while 'px on' is active (mode shell) and
'px shell'. Each row has the time, proxy, mode, exit code, duration, the
proxy latency measured during the command, the error class (curl's 7 is
"refused", 28 "timeout", ...) and, with --full, cwd, tty, session and the
preflight.

The same data is appended as text to <home>/logs/usage.log (history.text_log).
'px stats' aggregates it per proxy, command, error, day, ...`,
		Usage:     "[filters] [search...]",
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			histFilterFlags(fs, true)
			fs.Bool(&histOpts.full, "l,full", false, "show cwd, tty, session and preflight too")
			fs.Bool(&histOpts.json, "json", false, "print the rows as JSON")
			fs.Bool(&histOpts.logPath, "log-path", false, "print the text log's path and exit")
		},
		Examples: []cli.Example{
			{Cmd: "history", Desc: "the last 50 commands"},
			{Cmd: "history -c curl -s 24h --status fail", Desc: "curl failures today"},
			{Cmd: "history -p 42 -a", Desc: "everything proxy #42 was used for"},
			{Cmd: "history --session $SILKROUTER_SESSION", Desc: "this shell session"},
			{Cmd: "log --json | jq '.[0]'"},
		},
		Run: runHistory,
		Subcommands: []cli.Command{
			{
				Name:      "stats",
				Summary:   "aggregate the usage log (per proxy, command, error, day, ...)",
				Usage:     "[filters]",
				NeedsHome: true,
				Flags:     statsFlags,
				Run:       runStats,
			},
			{
				Name:      "export",
				Summary:   "export the usage log as CSV or JSON",
				Usage:     "[filters]",
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					histFilterFlags(fs, false)
					fs.String(&histOpts.format, "format", "csv", "output format").Placeholder("FMT").Choices("csv", "json")
					fs.String(&histOpts.output, "o,output", "", "write to FILE instead of stdout").Placeholder("FILE").Files()
				},
				Run: runHistoryExport,
			},
			{
				Name:    "prune",
				Summary: "delete usage rows older than a time (history.retention does it daily)",
				Usage:   "--older-than WHEN",
				Flags: func(fs *cli.FlagSet) {
					fs.String(&histOpts.older, "older-than", "", "delete rows that started before this (90d, 2026-01-01)").Placeholder("WHEN")
					fs.Bool(&histOpts.yes, "y,yes", false, "do not ask for confirmation")
				},
				NeedsHome: true,
				Run:       runHistoryPrune,
			},
		},
	})
	cli.Register(cli.Command{
		Name:    "stats",
		Group:   cli.GroupHistory,
		Summary: "usage statistics: uses, ok / failed, errors, latency per proxy / command / time",
		Description: `Aggregates the usage log. --by takes one or more keys (comma-separated):
proxy, cmd, err, mode, session, tty, cwd, day, hour, week, month. Every row
shows uses, ok (exit 0), failed, proxy / network errors, success rate,
average latency measured during the commands, average duration, last use and
the error classes seen.

  px stats                       per proxy
  px stats --by cmd              per command
  px stats --by proxy,cmd -s 7d  which commands ran on which proxy this week
  px stats --by day              uses per day
  px stats --by err -c curl      why curl failed`,
		Usage:     "[filters]",
		NeedsHome: true,
		Flags:     statsFlags,
		Run:       runStats,
	})
	cli.Register(cli.Command{
		Name:      "__shell-log",
		Hidden:    true,
		Summary:   "record a command typed while the proxy is on (used by the shell hooks)",
		Usage:     "--proxy URL --start MS --end MS --exit N [--session ID] [--cwd DIR] [--tty TTY] -- <command line>",
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			o := &histOpts
			fs.String(&o.shProxy, "proxy", "", "proxy URL")
			fs.String(&o.shSess, "session", "", "session id")
			fs.Var((*int64Value)(&o.shStart), "start", false, "start, unix ms")
			fs.Var((*int64Value)(&o.shEnd), "end", false, "end, unix ms")
			fs.Int(&o.shExit, "exit", 0, "exit code")
			fs.String(&o.shCwd, "cwd", "", "working directory")
			fs.String(&o.shTTY, "tty", "", "terminal")
		},
		Run: runShellLog,
	})
	cli.RegisterCompleter("used-commands", func(ctx *cli.Context) []cli.Candidate {
		var out []cli.Candidate
		if _, err := os.Stat(ctx.Config.DBFile()); err != nil {
			return nil
		}
		d, err := db.OpenWith(ctx.Config.DBFile(), db.Options{ReadOnly: true})
		if err != nil {
			return nil
		}
		defer d.Close()
		gs, _ := d.UsageStats(ctx, []string{"cmd"}, db.UsageFilter{}, "uses", 200)
		for _, g := range gs {
			out = append(out, cli.Candidate{Value: g.Key[0], Desc: fmt.Sprintf("%d uses", g.Uses)})
		}
		return out
	})
}

type int64Value int64

func (v *int64Value) Set(s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid integer %q", s)
	}
	*v = int64Value(n)
	return nil
}
func (v *int64Value) String() string { return strconv.FormatInt(int64(*v), 10) }

func statsFlags(fs *cli.FlagSet) {
	histFilterFlags(fs, false)
	fs.String(&histOpts.by, "by", "proxy", "group by (comma-separated): "+strings.Join(db.UsageGroupKeys, ", ")).
		Placeholder("KEYS").Choices(db.UsageGroupKeys...)
	fs.String(&histOpts.sort, "sort", "uses", "order").Placeholder("KEY").Choices("uses", "last", "fail", "rate", "key")
	fs.Int(&histOpts.limit, "n,limit", 30, "show at most N groups (0 = all)")
	fs.Bool(&histOpts.json, "json", false, "print the groups as JSON")
}

func runHistory(ctx *cli.Context, args []string) int {
	if histOpts.logPath {
		fmt.Fprintln(ctx.Stdout, paths.Pretty(usageLogPath()))
		return 0
	}
	return withDB(ctx, func(d *db.DB) int {
		f, err := histOpts.build(ctx, d, args)
		if err != nil {
			return ctx.UsageError("%v", err)
		}
		rows, err := d.ListUsage(ctx, f)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if histOpts.json {
			if rows == nil {
				rows = []db.Usage{}
			}
			return ctx.JSON(rows)
		}
		printUsageTable(ctx, rows, histOpts.full)
		if f.Limit > 0 && len(rows) == f.Limit && !ctx.Quiet {
			total, _ := d.CountUsage(ctx, f)
			c := ctx.ErrColors()
			fmt.Fprintf(ctx.Stderr, "%sshowing %d-%d of %d (-n N, --offset N or -a for more)%s\n",
				c.Gray, f.Offset+1, f.Offset+len(rows), total, c.Reset)
		}
		return 0
	})
}

func usageLogPath() string { return paths.LogsDir() + string(os.PathSeparator) + "usage.log" }

func fmtDurMS(ms int64) string {
	switch {
	case ms <= 0:
		return "-"
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm%02ds", ms/60_000, (ms/1000)%60)
	default:
		return fmt.Sprintf("%dh%02dm", ms/3_600_000, (ms/60_000)%60)
	}
}

func printUsageTable(ctx *cli.Context, rows []db.Usage, full bool) {
	c := ctx.Colors()
	if len(rows) == 0 {
		fmt.Fprintf(ctx.Stdout, "%sno usage recorded yet%s\n", c.Gray, c.Reset)
		return
	}
	w := 0
	for _, u := range rows {
		w = max(w, len(u.ProxyURL))
	}
	w = min(max(w, 5), 34)
	fmt.Fprintf(ctx.Stdout, "%s%-19s  %-5s  %-*s  %4s  %7s  %6s  %-12s  %s%s\n", c.Bold, "TIME", "MODE", w, "PROXY",
		"EXIT", "TOOK", "MS", "ERROR", "COMMAND", c.Reset)
	for _, u := range rows {
		code, cc := "…", c.Cyan
		if u.ExitCode != nil {
			code = strconv.Itoa(*u.ExitCode)
			cc = c.Green
			if *u.ExitCode != 0 {
				cc = c.Red
			}
		}
		errc := c.Gray
		if db.IsProxyErr(u.ErrClass) {
			errc = c.Red
		} else if u.ErrClass != "" {
			errc = c.Yellow
		}
		proxyURL := u.ProxyURL
		if len(proxyURL) > w {
			proxyURL = proxyURL[:w-1] + "…"
		}
		cmdline := strings.Join(u.Argv, " ")
		if !full && len(u.Argv) > 0 && u.Cmd != "" {
			cmdline = strings.Join(append([]string{u.Cmd}, u.Argv[1:]...), " ")
		}
		if lim := ui80(ctx) - 70 - w; lim > 20 && len(cmdline) > lim && !full {
			cmdline = cmdline[:lim-1] + "…"
		}
		fmt.Fprintf(ctx.Stdout, "%s%-19s%s  %-5s  %s%-*s%s  %s%4s%s  %7s  %s%6s%s  %s%-12s%s  %s\n",
			c.Gray, u.TSStart.Local().Format("2006-01-02 15:04:05"), c.Reset, u.Mode, c.Cyan, w, proxyURL, c.Reset,
			cc, code, c.Reset, fmtDurMS(u.DurationMS), msColor(c, u.MS), msOrDash(u.MS), c.Reset,
			errc, dash(u.ErrClass), c.Reset, cmdline)
		if full {
			pre := "-"
			if u.PreflightOK != nil {
				pre = fmt.Sprintf("%v", *u.PreflightOK)
				if u.PreflightMS != nil && *u.PreflightMS > 0 {
					pre += fmt.Sprintf(" %dms", *u.PreflightMS)
				}
			}
			fmt.Fprintf(ctx.Stdout, "  %s#%d cwd=%s tty=%s session=%s pid=%d preflight=%s%s\n", c.Gray, u.ID,
				dash(paths.Pretty(u.Cwd)), dash(u.TTY), dash(u.SessionID), u.PID, pre, c.Reset)
		}
	}
}

func ui80(ctx *cli.Context) int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 40 {
		return n
	}
	return 160
}

func runStats(ctx *cli.Context, args []string) int {
	return withDB(ctx, func(d *db.DB) int {
		f, err := histOpts.build(ctx, d, args)
		if err != nil {
			return ctx.UsageError("%v", err)
		}
		f.Limit, f.Offset = 0, 0
		by := splitList(histOpts.by)
		groups, err := d.UsageStats(ctx, by, f, histOpts.sort, histOpts.limit)
		if err != nil {
			return ctx.UsageError("%v", err)
		}
		if histOpts.json {
			if groups == nil {
				groups = []db.UsageGroup{}
			}
			return ctx.JSON(groups)
		}
		c := ctx.Colors()
		if len(groups) == 0 {
			fmt.Fprintf(ctx.Stdout, "%sno usage recorded%s\n", c.Gray, c.Reset)
			return 0
		}
		widths := make([]int, len(by))
		for i, k := range by {
			widths[i] = len(k)
			for _, g := range groups {
				widths[i] = min(max(widths[i], len(g.Key[i])), 40)
			}
		}
		var head strings.Builder
		for i, k := range by {
			fmt.Fprintf(&head, "%-*s  ", widths[i], strings.ToUpper(k))
		}
		fmt.Fprintf(ctx.Stdout, "%s%s%6s  %6s  %6s  %6s  %5s  %7s  %8s  %-8s  %s%s\n", c.Bold, head.String(), "USES", "OK", "FAIL",
			"PROXY", "RATE", "AVG MS", "AVG TOOK", "LAST", "ERRORS", c.Reset)
		for _, g := range groups {
			var key strings.Builder
			for i, k := range g.Key {
				if len(k) > widths[i] {
					k = k[:widths[i]-1] + "…"
				}
				fmt.Fprintf(&key, "%s%-*s%s  ", c.Cyan, widths[i], k, c.Reset)
			}
			rate, rc := "-", c.Gray
			if r := g.Rate(); r >= 0 {
				rate = fmt.Sprintf("%.0f%%", 100*r)
				switch {
				case r >= 0.9:
					rc = c.Green
				case r >= 0.6:
					rc = c.Yellow
				default:
					rc = c.Red
				}
			}
			avg := "-"
			if g.AvgMS > 0 {
				avg = strconv.Itoa(int(g.AvgMS + 0.5))
			}
			fmt.Fprintf(ctx.Stdout, "%s%6d  %s%6d%s  %s%6d%s  %s%6d%s  %s%5s%s  %s%7s%s  %8s  %s%-8s%s  %s\n",
				key.String(), g.Uses, c.Green, g.OK, c.Reset, c.Red, g.Fail, c.Reset, c.Yellow, g.ProxyErr, c.Reset,
				rc, rate, c.Reset, msColor(c, int(g.AvgMS)), avg, c.Reset, fmtDurMS(int64(g.AvgDurMS)),
				c.Gray, ago(g.Last), c.Reset, fmtTagCounts(g.ErrClasses))
		}
		return 0
	})
}

func runHistoryExport(ctx *cli.Context, args []string) int {
	return withDB(ctx, func(d *db.DB) int {
		f, err := histOpts.build(ctx, d, args)
		if err != nil {
			return ctx.UsageError("%v", err)
		}
		f.Limit = 0
		rows, err := d.ListUsage(ctx, f)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		out := ctx.Stdout
		if histOpts.output != "" {
			file, err := os.Create(paths.ExpandUser(histOpts.output))
			if err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
			defer file.Close()
			out = file
		}
		if histOpts.format == "json" {
			if rows == nil {
				rows = []db.Usage{}
			}
			saved := ctx.Stdout
			ctx.Stdout = out
			code := ctx.JSON(rows)
			ctx.Stdout = saved
			return code
		}
		w := csv.NewWriter(out)
		_ = w.Write([]string{"id", "ts_start", "ts_end", "mode", "proxy_id", "proxy_url", "cmd", "argv", "cwd", "tty",
			"session_id", "pid", "preflight_ok", "preflight_ms", "exit_code", "duration_ms", "ms", "err_class"})
		for _, u := range rows {
			ptr := func(p *int) string {
				if p == nil {
					return ""
				}
				return strconv.Itoa(*p)
			}
			pok := ""
			if u.PreflightOK != nil {
				pok = strconv.FormatBool(*u.PreflightOK)
			}
			end := ""
			if !u.TSEnd.IsZero() {
				end = u.TSEnd.Format(time.RFC3339Nano)
			}
			_ = w.Write([]string{strconv.FormatInt(u.ID, 10), u.TSStart.Format(time.RFC3339Nano), end, u.Mode,
				strconv.FormatInt(u.ProxyID, 10), u.ProxyURL, u.Cmd, strings.Join(u.Argv, " "), u.Cwd, u.TTY, u.SessionID,
				strconv.Itoa(u.PID), pok, ptr(u.PreflightMS), ptr(u.ExitCode), strconv.FormatInt(u.DurationMS, 10),
				strconv.Itoa(u.MS), u.ErrClass})
		}
		w.Flush()
		if err := w.Error(); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		ctx.Infof("exported %d usage rows", len(rows))
		return 0
	})
}

func runHistoryPrune(ctx *cli.Context, _ []string) int {
	if histOpts.older == "" {
		return ctx.UsageError("--older-than is required")
	}
	before, err := history.ParseSince(histOpts.older, time.Now())
	if err != nil {
		return ctx.UsageError("%v", err)
	}
	return withDB(ctx, func(d *db.DB) int {
		n, err := d.CountUsage(ctx, db.UsageFilter{Until: before})
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if n == 0 {
			ctx.Infof("nothing older than %s", before.Local().Format("2006-01-02 15:04"))
			return 0
		}
		if !histOpts.yes {
			if f, ok := ctx.Stdin.(*os.File); !ok || !ui.IsTerminal(f) {
				ctx.Errorf("refusing to delete %d rows without --yes (stdin is not a terminal)", n)
				return 1
			}
			if !confirm(ctx.Stdin, ctx.Stderr, fmt.Sprintf("Delete %d usage rows older than %s?", n, before.Local().Format("2006-01-02 15:04"))) {
				return 1
			}
		}
		deleted, err := d.PruneUsage(ctx, before)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		ctx.Infof("deleted %d usage rows (proxy counters keep counting them; 'px db rebuild-stats' recounts)", deleted)
		return 0
	})
}

// runShellLog records one command typed while the proxy was on. It is run
// in the background by the shell hooks, so it never prints on success.
func runShellLog(ctx *cli.Context, args []string) int {
	o := &histOpts
	line := strings.TrimSpace(strings.Join(args, " "))
	if line == "" || o.shProxy == "" {
		return 2
	}
	argv := strings.Fields(line)
	cmd := strings.TrimSuffix(argv[0], ".exe")
	for _, skip := range ctx.Config.History.SkipCommands {
		if cmd == skip {
			return 0
		}
	}
	d, err := openDB(ctx)
	if err != nil {
		return 1
	}
	defer d.Close()
	start, end := time.UnixMilli(o.shStart), time.UnixMilli(o.shEnd)
	if o.shStart <= 0 {
		start = time.Now()
	}
	if o.shEnd <= 0 || end.Before(start) {
		end = start
	}
	u := db.Usage{TSStart: start.UTC(), TSEnd: end.UTC(), ProxyURL: o.shProxy, Mode: db.ModeShell, Argv: argv,
		Cwd: o.shCwd, TTY: o.shTTY, SessionID: o.shSess, PPID: os.Getppid()}
	if p, err := d.GetProxyByURL(ctx, o.shProxy); err == nil {
		u.ProxyID = p.ID
		u.ProxyURL = p.Key()
	}
	code := o.shExit
	u.ExitCode = &code
	u.ErrClass = history.ClassifyExit(cmd, code)
	if _, err := d.RecordUsage(ctx, u); err != nil {
		return 1
	}
	if ctx.Config.History.TextLog {
		u.DurationMS = end.Sub(start).Milliseconds()
		_ = history.AppendTextLog(usageLogPath(), u)
	}
	return 0
}
