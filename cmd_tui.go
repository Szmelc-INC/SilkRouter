package main

import (
	"os"
	"strings"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/tui"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

var tuiOpts struct {
	tab      string
	theme    string
	refresh  config.Duration
	noMouse  bool
	noAlt    bool
	search   string
	sort     string
	reverse  bool
	status   string
	showDead bool
}

func init() {
	cli.Register(cli.Command{
		Name:    "tui",
		Aliases: []string{"ui"},
		Group:   cli.GroupTUI,
		Summary: "interactive terminal UI: browse and manage proxies, history, tags, scans, lists and config",
		Description: `A full-screen, keyboard and mouse driven interface over the database.
Seven tabs (keys 1-7, tab / shift+tab):

  1 Dashboard  totals by status and scheme, alive latency, facts, tags,
               recent scans, top commands, the proxy of this shell session
  2 Proxies    the proxy table (columns: tui.columns), live search (/),
               sort (s, r), filters (a status, p scheme, D dead rows),
               details (enter), tag (t / T), block (b), check now (c),
               copy the URL with OSC 52 (y)
  3 History    the usage log with filters; g cycles stats by proxy, cmd,
               err, day, ... (px stats)
  4 Tags       heuristic and manual tags; enter lists the proxies, R
               recomputes the heuristic tags
  5 Scans      scan runs and their progress; k recomputes the ranks
  6 Lists      reputation lists (px lists); u downloads stale ones
  7 Config     every key with value, origin and default; enter edits it
               in place and writes config.toml like 'config set'

':' opens the command palette (tab completes): :tab NAME, :sort KEY
[asc|desc], :filter status=alive scheme=socks tag=fast max_ms=500,
:search TEXT, :tag add|rm NAME, :block, :unblock, :check, :rank [MODE],
:refresh, :export FILE, :theme NAME, :set KEY VALUE, :help, :q. '?'
shows every key of the current tab (markdown via glamour when
tui.markdown is on).

The visible tab reloads every tui.refresh; queries and checks run in the
background, so the interface never blocks. --no-color / NO_COLOR give a
plain, uncolored interface. Needs an interactive terminal; see
docs/TUI.md.`,
		Usage:     "[options] [search...]",
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			cfg := fs.Config
			o := &tuiOpts
			fs.String(&o.tab, "t,tab", cfg.TUI.StartTab, "tab shown on start (name or 1-7)").
				Placeholder("TAB").Choices(tui.TabNames()...).ConfigKey("tui.start_tab")
			fs.String(&o.theme, "theme", cfg.TUI.Theme, "color theme").
				Placeholder("NAME").Choices(tui.ThemeNames()...).ConfigKey("tui.theme")
			fs.Duration(&o.refresh, "refresh", cfg.TUI.Refresh, "reload the visible tab this often (0 = manual)").
				ConfigKey("tui.refresh")
			fs.Bool(&o.noMouse, "no-mouse", !cfg.TUI.Mouse, "disable mouse support (wheel, clicks)").ConfigKey("tui.mouse")
			fs.Bool(&o.noAlt, "no-alt-screen", !cfg.TUI.AltScreen, "draw in the normal screen instead of the alternate one").
				ConfigKey("tui.alt_screen")
			fs.String(&o.search, "S,search", "", "initial proxy search (host, URL, exit IP, country, org, source)").
				Placeholder("TEXT")
			fs.String(&o.sort, "sort", cfg.TUI.DefaultSort, "initial sort of the proxy table (natural direction; -r reverses)").
				Placeholder("KEY").Choices(db.SortKeys()...).ConfigKey("tui.default_sort")
			fs.Bool(&o.reverse, "r,reverse", false, "reverse the initial sort")
			fs.String(&o.status, "status", "all", "initial status filter of the proxy table").
				Placeholder("STATUS").Choices(tui.StatusFilters...)
			fs.Bool(&o.showDead, "dead,show-dead", cfg.TUI.ShowDeadRows, "show dead proxies while the status filter is all").
				ConfigKey("tui.show_dead")
		},
		Examples: []cli.Example{
			{Cmd: "tui", Desc: "open the dashboard (tui.start_tab)"},
			{Cmd: "ui --tab proxies --status alive --sort last_ms", Desc: "the fastest working proxies"},
			{Cmd: "tui -t history", Desc: "straight to the usage log"},
			{Cmd: "tui -S 185.199 --dead", Desc: "search, dead rows included"},
			{Cmd: "tui --theme neon --refresh 2s"},
			{Cmd: "--no-color tui --no-mouse --no-alt-screen", Desc: "plain, inline, keyboard only"},
		},
		Run: runTUI,
	})
}

func runTUI(ctx *cli.Context, args []string) int {
	o := &tuiOpts
	if !contains(tui.TabNames(), strings.ToLower(o.tab)) && !(len(o.tab) == 1 && o.tab >= "1" && o.tab <= "7") {
		return ctx.UsageError("unknown tab %q (want one of: %s, or 1-7)", o.tab, strings.Join(tui.TabNames(), ", "))
	}
	if !contains(tui.ThemeNames(), o.theme) {
		return ctx.UsageError("unknown theme %q (want one of: %s)", o.theme, strings.Join(tui.ThemeNames(), ", "))
	}
	// A bad tui.default_sort in the file falls back to rank inside the TUI;
	// only a bad --sort is a usage error.
	if ctx.Flags.Changed("sort") && !contains(db.SortKeys(), o.sort) {
		return ctx.UsageError("unknown sort key %q (want one of: %s)", o.sort, strings.Join(db.SortKeys(), ", "))
	}
	if !contains(tui.StatusFilters, o.status) {
		return ctx.UsageError("unknown status %q (want one of: %s)", o.status, strings.Join(tui.StatusFilters, ", "))
	}
	in, inOK := ctx.Stdin.(*os.File)
	out, outOK := ctx.Stdout.(*os.File)
	if !inOK || !outOK || !ui.IsTerminal(in) || !ui.IsTerminal(out) {
		ctx.Errorf("the TUI needs an interactive terminal (stdin and stdout); for scripts use '%s proxies', '%s history', '%s stats', ...",
			ctx.Prog, ctx.Prog, ctx.Prog)
		return 1
	}
	return withDB(ctx, func(d *db.DB) int {
		err := tui.Run(ctx, tui.Options{
			DB:           d,
			Config:       ctx.Config,
			ConfigPath:   ctx.ConfigPath,
			Prog:         ctx.Prog,
			Version:      version,
			Tab:          o.tab,
			Theme:        o.theme,
			Search:       strings.TrimSpace(strings.Join(append(args, o.search), " ")),
			Sort:         o.sort,
			Reverse:      o.reverse,
			Status:       o.status,
			ShowDead:     o.showDead,
			Refresh:      o.refresh.D(),
			Mouse:        !o.noMouse,
			AltScreen:    !o.noAlt,
			Color:        ui.Mode(),
			ScoreOptions: scoreOptions,
			TagOptions:   tagOptions,
			ListManager:  listManager,
			// Input / Output: the process's stdin / stdout (Bubble Tea's
			// defaults, raw mode and all).
		})
		if err != nil {
			ctx.Errorf("tui: %v", err)
			return 1
		}
		return 0
	})
}
