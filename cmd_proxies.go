package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

// filterOpts are the proxy filter / sort flags shared by `proxies` and
// `db export`.
type filterOpts struct {
	status    string
	scheme    string
	socks     bool
	http      bool
	maxMS     int
	minMS     int
	untimed   bool
	tag       string
	country   string
	listedMax int
	minStreak int
	source    string
	search    string
	sort      string
	reverse   bool
	limit     int
	offset    int
	all       bool
}

// filterFlags declares the filter flags. paging adds -n/--offset/--all
// with db.list_limit as the default limit.
func filterFlags(fs *cli.FlagSet, o *filterOpts, paging bool) {
	fs.String(&o.status, "status", "", "only these statuses (comma-separated)").
		Placeholder("LIST").Choices(db.Statuses...)
	fs.String(&o.scheme, "scheme", "", "only these schemes (comma-separated; socks = socks4/4a/5)").
		Placeholder("LIST").Choices("http", "https", "socks", "socks4", "socks4a", "socks5")
	fs.Bool(&o.socks, "s,socks", false, "socks proxies only")
	fs.Bool(&o.http, "H,http", false, "http/https proxies only")
	fs.Int(&o.maxMS, "d,max-ms", 0, "maximum last latency (0 = no limit)").Placeholder("MS")
	fs.Int(&o.minMS, "D,min-ms", 0, "minimum last latency").Placeholder("MS")
	fs.Bool(&o.untimed, "untimed", false, "let proxies without a latency pass -d")
	fs.String(&o.tag, "tag", "", "only proxies with this tag (TAG or TAG:SCOPE)").Placeholder("TAG")
	fs.String(&o.country, "country", "", "only these exit countries (comma-separated codes)").Placeholder("CC")
	fs.Int(&o.listedMax, "listed-max", -1, "at most N reputation listings (-1 = any)").Placeholder("N")
	fs.Int(&o.minStreak, "min-streak", 0, "at least N consecutive successes").Placeholder("N")
	fs.String(&o.source, "source", "", "only proxies imported from this source").Placeholder("NAME")
	fs.String(&o.search, "S,search", "", "substring of host, URL, exit IP, country, org or source").Placeholder("TEXT")
	defSort := ""
	if paging {
		defSort = "last_ms"
	}
	fs.String(&o.sort, "sort", defSort, "sort key (natural direction; -r reverses)").
		Placeholder("KEY").Choices(db.SortKeys()...)
	fs.Bool(&o.reverse, "r,reverse", false, "reverse the sort direction")
	limit := 0
	if paging {
		limit = fs.Config.DB.ListLimit
	}
	fs.Int(&o.limit, "n,limit", limit, "show at most N rows (0 = all)").Placeholder("N").ConfigKey(limitKey(paging))
	fs.Int(&o.offset, "offset", 0, "skip the first N rows").Placeholder("N")
	if paging {
		fs.Bool(&o.all, "a,all", false, "no row limit (same as -n 0)")
	}
}

func limitKey(paging bool) string {
	if paging {
		return "db.list_limit"
	}
	return ""
}

// build turns the flags (plus positional search words) into a db.Filter.
func (o *filterOpts) build(args []string) (db.Filter, error) {
	f := db.Filter{
		MinMS: o.minMS, MaxMS: o.maxMS, IncludeUntimed: o.untimed, MinOKStreak: o.minStreak,
		Source: o.source, Sort: o.sort, Reverse: o.reverse, Limit: o.limit, Offset: o.offset,
	}
	if o.all {
		f.Limit = 0
	}
	if o.status != "" {
		for _, s := range strings.Split(o.status, ",") {
			s = strings.TrimSpace(s)
			if !contains(db.Statuses, s) {
				return f, fmt.Errorf("unknown status %q (want %s)", s, strings.Join(db.Statuses, ", "))
			}
			f.Status = append(f.Status, s)
		}
	}
	var schemes []string
	if o.scheme != "" {
		schemes = append(schemes, o.scheme)
	}
	if o.socks {
		schemes = append(schemes, "socks")
	}
	f.Schemes = db.ExpandSchemes(schemes, false)
	if o.http {
		f.Schemes = append(f.Schemes, db.ExpandSchemes([]string{"http"}, true)...)
	}
	if o.tag != "" {
		f.Tag, f.TagScope, _ = strings.Cut(o.tag, ":")
	}
	if o.country != "" {
		for _, c := range strings.Split(o.country, ",") {
			if c = strings.TrimSpace(c); c != "" {
				f.Countries = append(f.Countries, c)
			}
		}
	}
	if o.listedMax >= 0 {
		n := o.listedMax
		f.ListedMax = &n
	}
	search := append([]string{}, args...)
	if o.search != "" {
		search = append(search, o.search)
	}
	f.Search = strings.Join(search, " ")
	if f.Sort != "" && !contains(db.SortKeys(), f.Sort) {
		return f, fmt.Errorf("unknown sort key %q (want one of: %s)", f.Sort, strings.Join(db.SortKeys(), ", "))
	}
	return f, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---- px proxies / px show --------------------------------------------------------

var proxiesOpts struct {
	filter filterOpts
	json   bool
	urls   bool
	count  bool
	recent int
}

func init() {
	cli.Register(cli.Command{
		Name:    "proxies",
		Aliases: []string{"db-ls"},
		Group:   cli.GroupDB,
		Summary: "list proxies from the database (filter, sort, page)",
		Description: `Prints the proxies in the database as an aligned table: status, latest /
best / average latency, success and failure counts, current streak, age of
the last check, reputation listings and tags. Positional words (and -S)
search host, URL, exit IP, country, org and source.

Sort keys have a natural direction (latency ascending, scores and streaks
descending, checked_at newest first); -r reverses it and unknown values
always come last.`,
		Usage:     "[options] [search...]",
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			filterFlags(fs, &proxiesOpts.filter, true)
			fs.Bool(&proxiesOpts.json, "json", false, "print the rows as JSON")
			fs.Bool(&proxiesOpts.urls, "u,urls", false, "print only the proxy URLs (one per line)")
			fs.Bool(&proxiesOpts.count, "c,count", false, "print only the number of matching proxies")
		},
		Examples: []cli.Example{
			{Cmd: "proxies", Desc: "fastest first (unmeasured last), 50 rows"},
			{Cmd: "proxies --status alive --socks -d 300", Desc: "fast working socks proxies"},
			{Cmd: "proxies --sort ok_streak -n 20", Desc: "most reliable"},
			{Cmd: "proxies --status dead --sort checked_at --count"},
			{Cmd: "proxies 185.199 --json | jq '.[0]'", Desc: "search"},
			{Cmd: "proxies --status alive -u -a > alive.txt"},
		},
		Run: runProxies,
	})
	cli.Register(cli.Command{
		Name:    "show",
		Aliases: []string{"inspect"},
		Group:   cli.GroupDB,
		Summary: "show everything known about one proxy",
		Description: `PROXY is a database id ("42" or "#42"), a proxy URL
("socks5://1.2.3.4:1080") or a host[:port] that matches one proxy. Prints
every stored field, the most recent measurements, reputation verdicts for
its host and exit IP, its tags and its usage.`,
		Usage:     "[options] <proxy|id>",
		NeedsHome: true,
		Flags: func(fs *cli.FlagSet) {
			fs.Int(&proxiesOpts.recent, "n,recent", fs.Config.DB.ShowRecent, "recent measurements to show").
				Placeholder("N").ConfigKey("db.show_recent")
			fs.Bool(&proxiesOpts.json, "json", false, "print everything as JSON")
		},
		Examples: []cli.Example{
			{Cmd: "show 42"},
			{Cmd: "show socks5://1.2.3.4:1080 --json"},
		},
		Run: runShow,
	})
}

func runProxies(ctx *cli.Context, args []string) int {
	f, err := proxiesOpts.filter.build(args)
	if err != nil {
		return ctx.UsageError("%v", err)
	}
	f.WithTags = true
	return withDB(ctx, func(d *db.DB) int {
		if proxiesOpts.count {
			n, err := d.CountProxies(ctx, f)
			if err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
			if proxiesOpts.json {
				return ctx.JSON(map[string]int{"count": n})
			}
			fmt.Fprintln(ctx.Stdout, n)
			return 0
		}
		ps, err := d.ListProxies(ctx, f)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if proxiesOpts.json {
			if ps == nil {
				ps = []*db.Proxy{}
			}
			return ctx.JSON(ps)
		}
		if proxiesOpts.urls {
			for _, p := range ps {
				fmt.Fprintln(ctx.Stdout, p.URL())
			}
			return 0
		}
		printProxyTable(ctx, ps)
		if f.Limit > 0 && len(ps) == f.Limit && !ctx.Quiet {
			total, _ := d.CountProxies(ctx, f)
			c := ctx.ErrColors()
			fmt.Fprintf(ctx.Stderr, "%sshowing %d-%d of %d (-n N, --offset N or -a for more)%s\n",
				c.Gray, f.Offset+1, f.Offset+len(ps), total, c.Reset)
		}
		return 0
	})
}

func statusColor(c ui.Palette, s string) string {
	switch s {
	case db.StatusAlive:
		return c.Green
	case db.StatusDead:
		return c.Red
	default:
		return c.Gray
	}
}

func msColor(c ui.Palette, ms int) string {
	switch {
	case ms <= 0:
		return c.Gray
	case ms < 300:
		return c.Green
	case ms < 1000:
		return c.Yellow
	default:
		return c.Red
	}
}

func fmtMS(ms int) string {
	if ms <= 0 {
		return "-"
	}
	return strconv.Itoa(ms)
}

// printProxyTable prints an aligned, colored table. Widths are computed on
// the plain text, then colors are wrapped around the padded cells.
func printProxyTable(ctx *cli.Context, ps []*db.Proxy) {
	c := ctx.Colors()
	if len(ps) == 0 {
		fmt.Fprintf(ctx.Stdout, "%sno proxies match%s\n", c.Gray, c.Reset)
		return
	}
	head := []string{"ID", "PROXY", "STATUS", "LAST", "BEST", "AVG", "OK", "FAIL", "STREAK", "CHECKED", "BL", "TAGS"}
	right := map[int]bool{0: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true}
	type cell struct{ text, color string }
	rows := make([][]cell, 0, len(ps))
	for _, p := range ps {
		streak, sc := "0", c.Gray
		if p.OKStreak > 0 {
			streak, sc = "+"+strconv.Itoa(p.OKStreak), c.Green
		} else if p.FailStreak > 0 {
			streak, sc = "-"+strconv.Itoa(p.FailStreak), c.Red
		}
		avg := "-"
		if p.AvgMS > 0 {
			avg = strconv.Itoa(int(p.AvgMS + 0.5))
		}
		bl, blc := strconv.Itoa(p.ListedCount), c.Gray
		if p.ListedCount > 0 {
			blc = c.Red
		}
		rows = append(rows, []cell{
			{strconv.FormatInt(p.ID, 10), c.Gray},
			{p.Key(), c.Cyan},
			{p.Status, statusColor(c, p.Status)},
			{fmtMS(p.LastMS), msColor(c, p.LastMS)},
			{fmtMS(p.BestMS), msColor(c, p.BestMS)},
			{avg, msColor(c, int(p.AvgMS))},
			{strconv.Itoa(p.OKCount), ""},
			{strconv.Itoa(p.FailCount), ""},
			{streak, sc},
			{ago(p.CheckedAt), c.Gray},
			{bl, blc},
			{strings.Join(p.Tags, " "), c.Magenta},
		})
	}
	width := make([]int, len(head))
	for i, h := range head {
		width[i] = len(h)
	}
	for _, r := range rows {
		for i, cl := range r {
			width[i] = max(width[i], len(cl.text))
		}
	}
	pad := func(i int, s string) string {
		if i == len(head)-1 {
			return s
		}
		if right[i] {
			return fmt.Sprintf("%*s", width[i], s)
		}
		return fmt.Sprintf("%-*s", width[i], s)
	}
	var b strings.Builder
	for i, h := range head {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(c.Bold + pad(i, h) + c.Reset)
	}
	fmt.Fprintln(ctx.Stdout, strings.TrimRight(b.String(), " "))
	for _, r := range rows {
		b.Reset()
		for i, cl := range r {
			if i > 0 {
				b.WriteString("  ")
			}
			if cl.color != "" {
				b.WriteString(cl.color + pad(i, cl.text) + c.Reset)
			} else {
				b.WriteString(pad(i, cl.text))
			}
		}
		fmt.Fprintln(ctx.Stdout, strings.TrimRight(b.String(), " "))
	}
}

type showOutput struct {
	Proxy        *db.Proxy         `json:"proxy"`
	Measurements []db.Measurement  `json:"measurements"`
	Reputation   []db.Reputation   `json:"reputation"`
	Tags         []db.Tag          `json:"tags"`
	UsageCount   int               `json:"usage_count"`
	Usage        []db.Usage        `json:"usage"`
	ByKind       map[string][2]int `json:"by_kind"` // kind -> [ok, fail] in the shown window
}

func runShow(ctx *cli.Context, args []string) int {
	if len(args) != 1 {
		return ctx.UsageError("give exactly one proxy (id, URL or host[:port])")
	}
	return withDB(ctx, func(d *db.DB) int {
		p, err := d.FindProxy(ctx, args[0])
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		out := showOutput{Proxy: p, ByKind: map[string][2]int{}}
		if out.Measurements, err = d.Measurements(ctx, p.ID, proxiesOpts.recent); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		ips := []string{p.Host}
		if p.ExitIP != "" && p.ExitIP != p.Host {
			ips = append(ips, p.ExitIP)
		}
		out.Reputation, _ = d.ReputationFor(ctx, ips...)
		out.Tags, _ = d.Tags(ctx, p.ID)
		out.UsageCount, _ = d.UsageCount(ctx, p.ID)
		out.Usage, _ = d.ListUsage(ctx, db.UsageFilter{ProxyID: p.ID, Limit: 5})
		for _, m := range out.Measurements {
			v := out.ByKind[m.Kind]
			if m.OK {
				v[0]++
			} else {
				v[1]++
			}
			out.ByKind[m.Kind] = v
		}
		if proxiesOpts.json {
			if out.Measurements == nil {
				out.Measurements = []db.Measurement{}
			}
			if out.Reputation == nil {
				out.Reputation = []db.Reputation{}
			}
			if out.Tags == nil {
				out.Tags = []db.Tag{}
			}
			if out.Usage == nil {
				out.Usage = []db.Usage{}
			}
			return ctx.JSON(out)
		}
		printShow(ctx, &out)
		return 0
	})
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05") + " (" + ago(t) + " ago)"
}

func printShow(ctx *cli.Context, o *showOutput) {
	c := ctx.Colors()
	p := o.Proxy
	w := ctx.Stdout
	kv := func(k, v string) { fmt.Fprintf(w, "  %s%-16s%s %s\n", c.Cyan, k, c.Reset, v) }
	fmt.Fprintf(w, "%s%s%s  %s#%d%s  %s%s%s\n", c.Bold, p.Key(), c.Reset, c.Gray, p.ID, c.Reset,
		statusColor(c, p.Status), p.Status, c.Reset)
	if p.Username != "" {
		kv("auth", p.Username+":***")
	}
	kv("source", p.Source)
	kv("added", fmtTime(p.AddedAt))
	kv("seen in source", fmtTime(p.LastSeenInSource))
	kv("checked", fmtTime(p.CheckedAt))
	avg := "-"
	if p.AvgMS > 0 {
		avg = fmt.Sprintf("%.1fms over %d timed", p.AvgMS, p.MSCount)
	}
	kv("latency", fmt.Sprintf("last %s%s%s  best %s%s%s  avg %s",
		msColor(c, p.LastMS), msOrDash(p.LastMS), c.Reset, msColor(c, p.BestMS), msOrDash(p.BestMS), c.Reset, avg))
	rate := "-"
	if n := p.OKCount + p.FailCount; n > 0 {
		rate = fmt.Sprintf("%.0f%%", 100*float64(p.OKCount)/float64(n))
	}
	kv("checks", fmt.Sprintf("%s%d ok%s / %s%d failed%s (%s)", c.Green, p.OKCount, c.Reset, c.Red, p.FailCount, c.Reset, rate))
	kv("streak", fmt.Sprintf("%d ok, %d failed", p.OKStreak, p.FailStreak))
	kv("exit ip", dash(p.ExitIP))
	kv("country / asn", fmt.Sprintf("%s / %s %s", dash(p.Country), dash(p.ASN), p.Org))
	kv("anonymity", p.Anonymity)
	kv("listed on", fmt.Sprintf("%d list(s)", p.ListedCount))
	if p.SpeedScore != nil || p.CleanScore != nil || p.Rank != nil {
		kv("scores", fmt.Sprintf("speed %s  clean %s  rank %s", fptr(p.SpeedScore), fptr(p.CleanScore), iptr(p.Rank)))
	}
	if len(o.Tags) > 0 {
		var ls []string
		for _, t := range o.Tags {
			ls = append(ls, c.Magenta+t.Label()+c.Reset)
		}
		kv("tags", strings.Join(ls, " "))
	}

	fmt.Fprintf(w, "\n%sRecent measurements%s %s(%d shown)%s\n", c.Bold, c.Reset, c.Gray, len(o.Measurements), c.Reset)
	if len(o.Measurements) == 0 {
		fmt.Fprintf(w, "  %snone%s\n", c.Gray, c.Reset)
	}
	for _, m := range o.Measurements {
		res := c.Green + "ok  " + c.Reset
		if !m.OK {
			res = c.Red + "FAIL" + c.Reset
		}
		extra := m.ErrClass
		if m.ExitIP != "" {
			extra = strings.TrimSpace(extra + " exit " + m.ExitIP)
		}
		fmt.Fprintf(w, "  %s%s%s  %-9s %-12s %s %s%6s%s  %s\n", c.Gray, m.TS.Local().Format("2006-01-02 15:04:05"), c.Reset,
			m.Kind, m.Probe, res, msColor(c, m.MS), msOrDash(m.MS), c.Reset, extra)
	}
	if len(o.Reputation) > 0 {
		fmt.Fprintf(w, "\n%sReputation%s\n", c.Bold, c.Reset)
		for _, r := range o.Reputation {
			verdict := c.Green + "clean " + c.Reset
			if r.Listed {
				verdict = c.Red + "LISTED" + c.Reset
			}
			fmt.Fprintf(w, "  %s  %-15s %-24s %-10s %s%s%s\n", verdict, r.IP, r.ListName, r.ListKind, c.Gray, ago(r.CheckedAt), c.Reset)
		}
	}
	fmt.Fprintf(w, "\n%sUsage%s %s(%d commands)%s\n", c.Bold, c.Reset, c.Gray, o.UsageCount, c.Reset)
	for _, u := range o.Usage {
		code := "-"
		if u.ExitCode != nil {
			code = strconv.Itoa(*u.ExitCode)
		}
		fmt.Fprintf(w, "  %s%s%s  %-6s exit %-3s %s\n", c.Gray, u.TSStart.Local().Format("2006-01-02 15:04:05"), c.Reset,
			u.Mode, code, strings.Join(u.Argv, " "))
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fptr(f *float64) string {
	if f == nil {
		return "-"
	}
	return strconv.FormatFloat(*f, 'f', 2, 64)
}

func iptr(i *int) string {
	if i == nil {
		return "-"
	}
	return strconv.Itoa(*i)
}
