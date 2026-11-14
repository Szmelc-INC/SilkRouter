package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

var tagOpts struct {
	json   bool
	reason string
	urls   bool
	pick   bool
	limit  int
	all    bool
}

const heuristicHelp = `Heuristic tags (refreshed after every use and every scan, thresholds in
[tags]):

  stable              the last tags.stable_n (10) checks / uses all succeeded
  fast                the last tags.fast_n (10) timed successes all < tags.fast_ms (100ms)
  fav:<cmd>           used >= tags.fav_min_uses (10) times for <cmd>, >= 80% ok
  recommended         the tags.recommend_top (5) best ranked proxies
  recommended:<cmd>   the best proxies for <cmd> (usage success, speed, cleanliness)
  clean               full-scanned, on no list, anonymous or elite
  elite               the target sees neither your IP nor proxy headers
  https               TLS through the proxy works
  flaky               alive, but most checks fail
  tor / datacenter    a Tor exit / a hosting range
  known               on a known-proxy list (many sites block these)

Any other tag is manual and never touched by a refresh: "blocked" keeps a
proxy out of route (route.exclude_tags), "pinned" or your own labels can be
used with 'px --tag NAME <cmd>' and 'px proxies --tag NAME'.`

func init() {
	cli.Register(cli.Command{
		Name:        "tags",
		Aliases:     []string{"tag"},
		Group:       cli.GroupHistory,
		Summary:     "heuristic and manual tags (stable, fast, fav:<cmd>, recommended, ...)",
		Description: heuristicHelp,
		Flags: func(fs *cli.FlagSet) {
			fs.Bool(&tagOpts.json, "json", false, "print the tag counts as JSON")
		},
		NeedsHome: true,
		Run:       runTagCounts,
		Subcommands: []cli.Command{
			{
				Name:      "refresh",
				Summary:   "recompute every heuristic tag now",
				NeedsHome: true,
				Flags:     func(fs *cli.FlagSet) { fs.Bool(&tagOpts.json, "json", false, "print the result as JSON") },
				Run:       runTagRefresh,
			},
			{
				Name:      "add",
				Aliases:   []string{"set"},
				Summary:   "tag a proxy by hand (e.g. blocked, pinned)",
				Usage:     "[options] <proxy> <tag[:scope]>...",
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.String(&tagOpts.reason, "reason", "manual", "why (shown by px show)").Placeholder("TEXT")
				},
				Examples: []cli.Example{
					{Cmd: "tags add 42 blocked --reason 'captcha everywhere'"},
					{Cmd: "tags add socks5://1.2.3.4:1080 pinned work"},
				},
				Run: runTagAdd,
			},
			{
				Name:      "rm",
				Aliases:   []string{"remove", "del"},
				Summary:   "remove tags from a proxy",
				Usage:     "<proxy> <tag[:scope]>...",
				Args:      cli.ArgsDynamic("tags"),
				NeedsHome: true,
				Run:       runTagRm,
			},
			{
				Name:      "show",
				Aliases:   []string{"ls"},
				Summary:   "list the proxies carrying a tag",
				Usage:     "<tag[:scope]>",
				Args:      cli.ArgsDynamic("tags"),
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.Bool(&tagOpts.urls, "u,urls", false, "print only the URLs")
					fs.Bool(&tagOpts.json, "json", false, "print the rows as JSON")
				},
				Run: runTagShow,
			},
		},
		Examples: []cli.Example{
			{Cmd: "tags", Desc: "every tag and how many proxies carry it"},
			{Cmd: "tags show fast"},
			{Cmd: "tags add 42 blocked"},
			{Cmd: "tags refresh"},
		},
	})
	cli.Register(cli.Command{
		Name:    "favorites",
		Aliases: []string{"favs", "fav"},
		Group:   cli.GroupHistory,
		Summary: "favourite proxies per command (fav:<cmd>)",
		Description: `Lists the proxies tagged fav:<cmd>: used at least tags.fav_min_uses times
for that command with a success rate of at least tags.fav_min_rate. With a
command, only its favourites.`,
		Usage:     "[options] [cmd]",
		Args:      cli.ArgsDynamic("used-commands"),
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			fs.Bool(&tagOpts.urls, "u,urls", false, "print only the URLs")
			fs.Bool(&tagOpts.json, "json", false, "print the rows as JSON")
		},
		Examples: []cli.Example{{Cmd: "favorites"}, {Cmd: "favs curl -u"}},
		Run:      func(ctx *cli.Context, args []string) int { return runTagged(ctx, score.TagFav, args) },
	})
	cli.Register(cli.Command{
		Name:    "recommend",
		Aliases: []string{"rec", "best"},
		Group:   cli.GroupHistory,
		Summary: "recommended proxies, overall or for a command",
		Description: `Without a command: the best ranked proxies (recommended). With one: the
proxies tagged recommended:<cmd> (best usage success for it, then speed and
cleanliness), falling back to fav:<cmd>, then to the overall recommendation.
--pick prints a single URL (handy in scripts: ALL_PROXY=$(px rec --pick curl)).`,
		Usage:     "[options] [cmd]",
		Args:      cli.ArgsDynamic("used-commands"),
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			fs.Bool(&tagOpts.urls, "u,urls", false, "print only the URLs")
			fs.Bool(&tagOpts.pick, "pick", false, "print one URL (the first) and exit")
			fs.Int(&tagOpts.limit, "n,limit", 20, "show at most N")
			fs.Bool(&tagOpts.json, "json", false, "print the rows as JSON")
		},
		Examples: []cli.Example{{Cmd: "recommend"}, {Cmd: "rec git"}, {Cmd: "rec --pick yt-dlp"}},
		Run:      runRecommend,
	})
	cli.RegisterCompleter("tags", func(ctx *cli.Context) []cli.Candidate {
		var out []cli.Candidate
		seen := map[string]bool{}
		for _, t := range score.HeuristicTags {
			out = append(out, cli.Candidate{Value: t, Desc: "heuristic"})
			seen[t] = true
		}
		for _, t := range []string{"blocked", "pinned"} {
			out = append(out, cli.Candidate{Value: t, Desc: "manual"})
			seen[t] = true
		}
		if _, err := os.Stat(ctx.Config.DBFile()); err != nil {
			return out
		}
		d, err := db.OpenWith(ctx.Config.DBFile(), db.Options{ReadOnly: true})
		if err != nil {
			return out
		}
		defer d.Close()
		counts, _ := d.TagCounts(ctx)
		for _, tc := range counts {
			label := tc.Tag
			if tc.Scope != "" {
				label += ":" + tc.Scope
			}
			if !seen[label] {
				seen[label] = true
				out = append(out, cli.Candidate{Value: label, Desc: fmt.Sprintf("%d proxies", tc.Count)})
			}
		}
		return out
	})
}

func runTagCounts(ctx *cli.Context, _ []string) int {
	return withDB(ctx, func(d *db.DB) int {
		counts, err := d.TagCounts(ctx)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if tagOpts.json {
			if counts == nil {
				counts = []db.TagCount{}
			}
			return ctx.JSON(counts)
		}
		c := ctx.Colors()
		if len(counts) == 0 {
			fmt.Fprintf(ctx.Stdout, "%sno tags yet: they appear after scans and uses ('%s scan full', '%s tags refresh')%s\n",
				c.Gray, ctx.Prog, ctx.Prog, c.Reset)
			return 0
		}
		fmt.Fprintf(ctx.Stdout, "%s%-28s %7s  %s%s\n", c.Bold, "TAG", "PROXIES", "KIND", c.Reset)
		for _, tc := range counts {
			label := tc.Tag
			if tc.Scope != "" {
				label += ":" + tc.Scope
			}
			kind := c.Yellow + "manual" + c.Reset
			if score.IsHeuristic(tc.Tag) {
				kind = c.Gray + "heuristic" + c.Reset
			}
			fmt.Fprintf(ctx.Stdout, "%s%-28s%s %7d  %s\n", c.Magenta, label, c.Reset, tc.Count, kind)
		}
		return 0
	})
}

func runTagRefresh(ctx *cli.Context, _ []string) int {
	return withDB(ctx, func(d *db.DB) int {
		res, err := score.RefreshTags(ctx, d, tagOptions(ctx.Config))
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if tagOpts.json {
			return ctx.JSON(res)
		}
		ctx.Infof("set %d tags in %s: %s", res.Total, roundDur(res.Elapsed), fmtTagCounts(res.Counts))
		return 0
	})
}

func parseTagLabel(s string) (string, string) {
	tag, scope, _ := strings.Cut(strings.TrimSpace(s), ":")
	return tag, scope
}

func runTagAdd(ctx *cli.Context, args []string) int {
	if len(args) < 2 {
		return ctx.UsageError("give a proxy and at least one tag")
	}
	return withDB(ctx, func(d *db.DB) int {
		p, err := d.FindProxy(ctx, args[0])
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		var ts []db.Tag
		for _, a := range args[1:] {
			tag, scope := parseTagLabel(a)
			if tag == "" {
				return ctx.UsageError("empty tag")
			}
			if score.IsHeuristic(tag) {
				ctx.Infof("note: %q is a heuristic tag; the next refresh recomputes it", tag)
			}
			ts = append(ts, db.Tag{ProxyID: p.ID, Tag: tag, Scope: scope, Reason: tagOpts.reason})
		}
		if err := d.SetTags(ctx, ts); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		ctx.Infof("tagged %s (#%d): %s", p.Key(), p.ID, strings.Join(args[1:], " "))
		return 0
	})
}

func runTagRm(ctx *cli.Context, args []string) int {
	if len(args) < 2 {
		return ctx.UsageError("give a proxy and at least one tag")
	}
	return withDB(ctx, func(d *db.DB) int {
		p, err := d.FindProxy(ctx, args[0])
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		for _, a := range args[1:] {
			tag, scope := parseTagLabel(a)
			if err := d.ClearTag(ctx, p.ID, tag, scope); err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
		}
		ctx.Infof("untagged %s (#%d): %s", p.Key(), p.ID, strings.Join(args[1:], " "))
		return 0
	})
}

func runTagShow(ctx *cli.Context, args []string) int {
	if len(args) != 1 {
		return ctx.UsageError("give one tag (TAG or TAG:SCOPE)")
	}
	tag, scope := parseTagLabel(args[0])
	return withDB(ctx, func(d *db.DB) int {
		ps, err := d.ListProxies(ctx, db.Filter{Tag: tag, TagScope: scope, Sort: "rank", WithTags: true})
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		return printProxies(ctx, ps)
	})
}

func printProxies(ctx *cli.Context, ps []*db.Proxy) int {
	switch {
	case tagOpts.json:
		if ps == nil {
			ps = []*db.Proxy{}
		}
		return ctx.JSON(ps)
	case tagOpts.urls:
		for _, p := range ps {
			fmt.Fprintln(ctx.Stdout, p.URL())
		}
	default:
		printProxyTable(ctx, ps)
	}
	return 0
}

// runTagged lists favourites: grouped by command, or one command's.
func runTagged(ctx *cli.Context, tag string, args []string) int {
	return withDB(ctx, func(d *db.DB) int {
		scope := ""
		if len(args) > 0 {
			scope = args[0]
		}
		ps, err := d.ListProxies(ctx, db.Filter{Tag: tag, TagScope: scope, Sort: "rank", WithTags: true})
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if tagOpts.json || tagOpts.urls || scope != "" {
			if len(ps) == 0 && !tagOpts.json && !tagOpts.urls {
				ctx.Infof("no favourites for %s yet (used >= %d times with >= %.0f%% ok)", scope,
					ctx.Config.Tags.FavMinUses, 100*ctx.Config.Tags.FavMinRate)
				return 0
			}
			return printProxies(ctx, ps)
		}
		byCmd := map[string][]*db.Proxy{}
		for _, p := range ps {
			for _, t := range p.Tags {
				if cmd, ok := strings.CutPrefix(t, tag+":"); ok {
					byCmd[cmd] = append(byCmd[cmd], p)
				}
			}
		}
		if len(byCmd) == 0 {
			c := ctx.Colors()
			fmt.Fprintf(ctx.Stdout, "%sno favourites yet: a proxy becomes fav:<cmd> after %d uses for <cmd> with >= %.0f%% ok%s\n",
				c.Gray, ctx.Config.Tags.FavMinUses, 100*ctx.Config.Tags.FavMinRate, c.Reset)
			return 0
		}
		cmds := make([]string, 0, len(byCmd))
		for k := range byCmd {
			cmds = append(cmds, k)
		}
		sort.Strings(cmds)
		c := ctx.Colors()
		for i, cmd := range cmds {
			if i > 0 {
				fmt.Fprintln(ctx.Stdout)
			}
			fmt.Fprintf(ctx.Stdout, "%s%s%s %s(%d)%s\n", c.Bold, cmd, c.Reset, c.Gray, len(byCmd[cmd]), c.Reset)
			printProxyTable(ctx, byCmd[cmd])
		}
		return 0
	})
}

func runRecommend(ctx *cli.Context, args []string) int {
	return withDB(ctx, func(d *db.DB) int {
		var ps []*db.Proxy
		var err error
		source := "recommended"
		base := db.Filter{Status: []string{db.StatusAlive}, Sort: "rank", WithTags: true, Limit: tagOpts.limit,
			ExcludeTags: ctx.Config.Route.ExcludeTags}
		if len(args) > 0 {
			cmd := args[0]
			for _, try := range []struct{ tag, scope string }{{score.TagRecommended, cmd}, {score.TagFav, cmd}} {
				f := base
				f.Tag, f.TagScope = try.tag, try.scope
				if ps, err = d.ListProxies(ctx, f); err != nil {
					ctx.Errorf("%v", err)
					return 1
				}
				if len(ps) > 0 {
					source = try.tag + ":" + try.scope
					break
				}
			}
		}
		if len(ps) == 0 {
			f := base
			f.Tag = score.TagRecommended
			if ps, err = d.ListProxies(ctx, f); err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
			source = "recommended"
		}
		if len(ps) == 0 { // no tags yet: the rank alone
			if ps, err = d.ListProxies(ctx, base); err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
			source = "rank"
		}
		if tagOpts.pick {
			if len(ps) == 0 {
				ctx.Errorf("nothing to recommend yet (px scan full)")
				return 1
			}
			fmt.Fprintln(ctx.Stdout, ps[0].URL())
			return 0
		}
		if !tagOpts.json && !tagOpts.urls && !ctx.Quiet {
			c := ctx.ErrColors()
			fmt.Fprintf(ctx.Stderr, "%sfrom %s%s\n", c.Gray, source, c.Reset)
		}
		return printProxies(ctx, ps)
	})
}
