package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/proxy"
	"github.com/szmelc-inc/silkrouter/internal/source"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

// ---- shared helpers ------------------------------------------------------------

// textCacheMode reports whether a command works on the legacy text cache
// instead of the database: -f/--cache was given, or cache.file / $PX_LIST
// is set. Route additionally honours route.pool = cache.
func textCacheMode(ctx *cli.Context) bool {
	if ctx.Flags != nil && ctx.Flags.Lookup("f") != nil && ctx.Flags.Changed("f") {
		return true
	}
	return ctx.Config.Origin("cache.file") != config.FromDefault
}

// openDB opens (creating and migrating) the configured database and, on
// an empty database with db.auto_import, imports the embedded list and
// <home>/cache.list.
func openDB(ctx *cli.Context) (*db.DB, error) {
	cfg := ctx.Config
	d, err := db.OpenWith(cfg.DBFile(), db.Options{BusyTimeout: cfg.DB.BusyTimeout.D()})
	if err != nil {
		return nil, err
	}
	if cfg.DB.AutoImport {
		res, err := d.FirstRun(ctx, db.FirstRunOptions{Builtin: source.Builtin, CacheFile: paths.LegacyCache()})
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("first import: %w", err)
		}
		if res.Ran && res.Builtin.Added+res.Cache.Total > 0 {
			ctx.Infof("database: imported %d proxies (embedded list) and %d from %s in %s",
				res.Builtin.Added, res.Cache.Total, paths.Pretty(paths.LegacyCache()), roundDur(res.Elapsed))
		}
	}
	return d, nil
}

// withDB runs fn with an open database and maps errors to exit code 1.
func withDB(ctx *cli.Context, fn func(d *db.DB) int) int {
	d, err := openDB(ctx)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	defer d.Close()
	return fn(d)
}

// dbRecorder records probe results into the database through a Writer.
type dbRecorder struct {
	d *db.DB
	w *db.Writer
}

// newRecorder opens the database for recording build/recache results, or
// returns nil (with a warning) when the database cannot be opened:
// recording never breaks the legacy flow.
func newRecorder(ctx *cli.Context) *dbRecorder {
	d, err := openDB(ctx)
	if err != nil {
		ctx.Errorf("warning: not recording into the database: %v", err)
		return nil
	}
	w := d.NewWriter(ctx, db.WriterOptions{
		BatchSize:     ctx.Config.DB.BatchSize,
		FlushInterval: ctx.Config.DB.FlushInterval.D(),
		OnError: func(err error, n int) {
			ctx.Errorf("warning: database: %d rows not recorded: %v", n, err)
		},
	})
	return &dbRecorder{d: d, w: w}
}

// close flushes and closes; safe on nil.
func (r *dbRecorder) close() {
	if r == nil {
		return
	}
	r.w.Close()
	r.d.Close()
}

// ---- formatting helpers --------------------------------------------------------

func roundDur(d time.Duration) time.Duration {
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond)
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond)
	default:
		return d
	}
}

// ago renders a compact age ("5s", "3m", "2h", "4d"); "-" for zero.
func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// confirm asks a yes/no question that defaults to NO. Non-interactive
// input (EOF) is a no.
func confirm(r io.Reader, w io.Writer, prompt string) bool {
	fmt.Fprintf(w, "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(r).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// ---- px db -----------------------------------------------------------------------

var dbOpts struct {
	json     bool
	builtin  bool
	source   string
	asCache  bool
	noLat    bool
	probe    string
	format   string
	output   string
	status   bool
	yes      bool
	noBackup bool
	empty    bool
	filter   filterOpts
}

func dbJSONFlag(fs *cli.FlagSet, what string) {
	fs.Bool(&dbOpts.json, "json", false, "print "+what+" as JSON")
}

func init() {
	cli.Register(cli.Command{
		Name:    "db",
		Group:   cli.GroupDB,
		Summary: "manage the SQLite database of every known proxy",
		Description: `The database (silkrouter.db in the config home, or db.path) holds every
known proxy and everything learned about it: measurements, reputation,
usage and tags. Routing, build and recache use it unless a legacy text cache
is selected (-f FILE, $PX_LIST or cache.file).

On first use an empty database is filled from the embedded list and
<home>/cache.list (db.auto_import).`,
		Subcommands: []cli.Command{
			{
				Name:    "info",
				Aliases: []string{"stats", "status"},
				Summary: "show the database location, schema and contents",
				Flags:   func(fs *cli.FlagSet) { dbJSONFlag(fs, "the summary") },
				Run:     runDBInfo,
			},
			{
				Name:    "path",
				Summary: "print the database file path",
				Run: func(ctx *cli.Context, _ []string) int {
					fmt.Fprintln(ctx.Stdout, ctx.Config.DBFile())
					return 0
				},
			},
			{
				Name:    "import",
				Summary: "import proxies from a file, URL or the embedded list",
				Description: `Adds every proxy from FILE, URL (http/https list) or the embedded list
(--builtin) to the database; known proxies are kept (deduplicated by
scheme, host and port). Entries with a "[Nms]" latency (a legacy text
cache) also record that latency as a probe measurement; --cache treats
every entry as a working proxy, as cache.list files are.`,
				Usage:     "[options] [FILE|URL]",
				Args:      cli.ArgsFiles,
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.Bool(&dbOpts.builtin, "builtin", false, "import the list embedded in the binary")
					fs.String(&dbOpts.source, "source", "", "source label of new rows (default: builtin, file:NAME or url:HOST)").
						Placeholder("NAME")
					fs.Bool(&dbOpts.asCache, "cache", false, "treat every entry as alive (a legacy cache.list)")
					fs.Bool(&dbOpts.noLat, "no-latency", false, "ignore [Nms] latencies")
					fs.String(&dbOpts.probe, "probe", db.ProbeLegacy, "probe name recorded with imported latencies").Placeholder("NAME")
					dbJSONFlag(fs, "the result")
				},
				Examples: []cli.Example{
					{Cmd: "db import --builtin"},
					{Cmd: "db import my-proxies.txt --source mine"},
					{Cmd: "db import ~/old/cache.list --cache"},
					{Cmd: "db import https://example.com/list.txt"},
				},
				Run: runDBImport,
			},
			{
				Name:    "export",
				Summary: "export proxies (legacy list, URLs, CSV or JSON)",
				Description: `Writes the proxies matching the filters. The list format is the legacy
text cache ("scheme://host:port [Nms]", fastest first, latency = last
measured), so an export is a drop-in cache.list / $PX_LIST file.`,
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.String(&dbOpts.format, "format", db.FormatList, "output format").
						Placeholder("FMT").Choices(db.ExportFormats...)
					fs.String(&dbOpts.output, "o,output", "", "write to FILE instead of stdout (atomic)").
						Placeholder("FILE").Files()
					filterFlags(fs, &dbOpts.filter, false)
				},
				Examples: []cli.Example{
					{Cmd: "db export --status alive > cache.list", Desc: "a legacy cache of working proxies"},
					{Cmd: "db export --format csv -o proxies.csv"},
					{Cmd: "db export --format json --socks -d 500"},
				},
				Run: runDBExport,
			},
			{
				Name:      "vacuum",
				Summary:   "checkpoint the WAL and compact the file",
				NeedsHome: true,
				Run:       runDBVacuum,
			},
			{
				Name:    "rebuild-stats",
				Aliases: []string{"rebuild"},
				Summary: "recompute every proxy's summary columns from the facts",
				Description: `The summary columns on proxies (status, last/best/avg ms, counts,
streaks, exit IP, listed count) are updated together with every fact.
This recomputes all of them from the measurements and reputation tables,
e.g. after editing the database by hand.`,
				NeedsHome: true,
				Flags:     func(fs *cli.FlagSet) { dbJSONFlag(fs, "the result") },
				Run:       runDBRebuild,
			},
			{
				Name:    "migrate",
				Summary: "apply pending schema migrations (or show them)",
				Flags: func(fs *cli.FlagSet) {
					fs.Bool(&dbOpts.status, "status", false, "only show the applied / pending migrations")
					dbJSONFlag(fs, "the migrations")
				},
				Run: runDBMigrate,
			},
			{
				Name:    "reset",
				Summary: "delete the database (after a backup) and start over",
				Description: `Backs the database up to silkrouter.db.bak-<time> (unless --no-backup
or db.backup = false), deletes it and creates a fresh one, filled from the
embedded list and cache.list again unless --empty. Asks for confirmation;
--yes skips the question (required when stdin is not a terminal).`,
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.Bool(&dbOpts.yes, "y,yes", false, "do not ask for confirmation")
					fs.Bool(&dbOpts.noBackup, "no-backup", false, "skip the backup").ConfigKey("db.backup")
					fs.Bool(&dbOpts.empty, "empty", false, "do not re-import the embedded list / cache.list")
				},
				Run: runDBReset,
			},
		},
		Examples: []cli.Example{
			{Cmd: "db info"},
			{Cmd: "db export --status alive > cache.list"},
			{Cmd: "db reset --yes"},
		},
	})
}

type dbInfo struct {
	Path       string             `json:"path"`
	Exists     bool               `json:"exists"`
	SizeBytes  int64              `json:"size_bytes"`
	WALBytes   int64              `json:"wal_bytes"`
	Latest     int                `json:"latest_schema"`
	Migrations []db.MigrationInfo `json:"migrations,omitempty"`
	Stats      *db.Stats          `json:"stats,omitempty"`
	Meta       map[string]string  `json:"meta,omitempty"`
}

func runDBInfo(ctx *cli.Context, _ []string) int {
	path := ctx.Config.DBFile()
	info := dbInfo{Path: path, Latest: db.LatestVersion()}
	if _, err := os.Stat(path); err == nil {
		info.Exists = true
		// Read-only: info never creates or migrates anything.
		d, err := db.OpenWith(path, db.Options{ReadOnly: true, BusyTimeout: ctx.Config.DB.BusyTimeout.D()})
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		defer d.Close()
		if info.Migrations, err = d.Migrations(ctx); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		if v, _ := d.SchemaVersion(ctx); v > 0 {
			if info.Stats, err = d.Stats(ctx); err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
			info.Meta, _ = d.AllMeta(ctx)
		}
		info.SizeBytes, info.WALBytes = db.Size(path)
	}
	if dbOpts.json {
		return ctx.JSON(info)
	}
	c := ctx.Colors()
	kv := func(k, v string) { fmt.Fprintf(ctx.Stdout, "  %s%-14s%s %s\n", c.Cyan, k, c.Reset, v) }
	fmt.Fprintf(ctx.Stdout, "%sDatabase%s\n", c.Bold, c.Reset)
	kv("path", paths.Pretty(path))
	if !info.Exists {
		kv("status", c.Gray+"not created yet (created on first use)"+c.Reset)
		return 0
	}
	kv("size", fmt.Sprintf("%s (+ %s WAL)", humanBytes(info.SizeBytes), humanBytes(info.WALBytes)))
	cur := 0
	if info.Stats != nil {
		cur = info.Stats.SchemaVersion
	}
	schema := fmt.Sprintf("v%d", cur)
	if cur < info.Latest {
		schema += fmt.Sprintf(" %s(v%d pending: run '%s db migrate')%s", c.Yellow, info.Latest, ctx.Prog, c.Reset)
	}
	kv("schema", schema)
	s := info.Stats
	if s == nil {
		return 0
	}
	fmt.Fprintf(ctx.Stdout, "\n%sProxies%s\n", c.Bold, c.Reset)
	kv("total", fmt.Sprint(s.Proxies))
	kv("alive", c.Green+fmt.Sprint(s.ByStatus[db.StatusAlive])+c.Reset)
	kv("dead", c.Red+fmt.Sprint(s.ByStatus[db.StatusDead])+c.Reset)
	kv("unknown", c.Gray+fmt.Sprint(s.ByStatus[db.StatusUnknown])+c.Reset)
	kv("measured", fmt.Sprintf("%d (%d timed)", s.Measured, s.Timed))
	for _, sc := range proxy.AllSchemes {
		if n := s.ByScheme[string(sc)]; n > 0 {
			kv(string(sc), fmt.Sprint(n))
		}
	}
	srcs := make([]string, 0, len(s.BySource))
	for k := range s.BySource {
		srcs = append(srcs, k)
	}
	sort.Strings(srcs)
	var parts []string
	for _, k := range srcs {
		label := k
		if label == "" {
			label = "(none)"
		}
		parts = append(parts, fmt.Sprintf("%s %d", label, s.BySource[k]))
	}
	kv("sources", strings.Join(parts, ", "))
	fmt.Fprintf(ctx.Stdout, "\n%sAlive latency%s\n", c.Bold, c.Reset)
	kv("best", msOrDash(s.BestMS))
	kv("median", msOrDash(s.MedianMS))
	kv("< 100ms", fmt.Sprint(s.Latency.Under100))
	kv("0-1s", fmt.Sprint(s.Latency.To1s))
	kv("1-5s", fmt.Sprint(s.Latency.To5s))
	kv("5-10s", fmt.Sprint(s.Latency.To10s))
	kv("10s+", fmt.Sprint(s.Latency.Over10s))
	fmt.Fprintf(ctx.Stdout, "\n%sFacts%s\n", c.Bold, c.Reset)
	kv("measurements", fmt.Sprintf("%d (last %s ago)", s.Measurements, ago(s.LastMeasurement)))
	kv("reputation", fmt.Sprintf("%d verdicts, %d proxies listed", s.Reputation, s.Listed))
	kv("usage", fmt.Sprint(s.Usage))
	kv("tags", fmt.Sprint(s.Tags))
	kv("scan runs", fmt.Sprint(s.ScanRuns))
	return 0
}

func msOrDash(ms int) string {
	if ms <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dms", ms)
}

func runDBImport(ctx *cli.Context, args []string) int {
	if len(args) > 1 {
		return ctx.UsageError("at most one FILE or URL")
	}
	if len(args) == 0 && !dbOpts.builtin {
		return ctx.UsageError("give a FILE, a URL or --builtin")
	}
	if len(args) == 1 && dbOpts.builtin {
		return ctx.UsageError("--builtin and a FILE/URL are exclusive")
	}
	var origin source.Origin
	label := dbOpts.source
	switch {
	case dbOpts.builtin:
		origin.UseBuiltin = true
		if label == "" {
			label = db.SourceBuiltin
		}
	case isURL(args[0]):
		origin.URL = args[0]
		if label == "" {
			label = "url:" + urlHost(args[0])
		}
	default:
		origin.File = paths.ExpandUser(args[0])
		if label == "" {
			label = "file:" + filepath.Base(args[0])
		}
	}
	ps, err := origin.Candidates(ctx, ctx.Config.Source.Timeout.D())
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	if len(ps) == 0 {
		ctx.Errorf("no proxies in %s", origin.Describe())
		return 1
	}
	return withDB(ctx, func(d *db.DB) int {
		res, err := d.ImportProxies(ctx, ps, db.ImportOptions{
			Source: label, Latencies: !dbOpts.noLat, AllAlive: dbOpts.asCache, Probe: dbOpts.probe,
		})
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		_ = d.SetMeta(ctx, db.MetaLastImport, fmt.Sprintf("%s %d", label, time.Now().UnixMilli()))
		if dbOpts.json {
			return ctx.JSON(res)
		}
		c := ctx.Colors()
		fmt.Fprintf(ctx.Stdout, "imported %d proxies from %s: %s%d new%s, %d known, %d latencies recorded (%s)\n",
			res.Total, origin.Describe(), c.Green, res.Added, c.Reset, res.Existing, res.Measurements, roundDur(res.Elapsed))
		return 0
	})
}

func isURL(s string) bool {
	if _, err := os.Stat(s); err == nil {
		return false
	}
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func urlHost(s string) string {
	s = s[strings.Index(s, "://")+3:]
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

func runDBExport(ctx *cli.Context, args []string) int {
	f, err := dbOpts.filter.build(args)
	if err != nil {
		return ctx.UsageError("%v", err)
	}
	return withDB(ctx, func(d *db.DB) int {
		var w io.Writer = ctx.Stdout
		var tmp *os.File
		if dbOpts.output != "" {
			out := paths.ExpandUser(dbOpts.output)
			tmp, err = os.CreateTemp(filepath.Dir(out), ".silkrouter-export-*")
			if err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
			defer os.Remove(tmp.Name())
			w = tmp
		}
		n, err := d.Export(ctx, w, dbOpts.format, f)
		if err != nil {
			if tmp != nil {
				tmp.Close()
			}
			ctx.Errorf("%v", err)
			return 1
		}
		if tmp != nil {
			if err := tmp.Close(); err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
			if err := os.Rename(tmp.Name(), paths.ExpandUser(dbOpts.output)); err != nil {
				ctx.Errorf("%v", err)
				return 1
			}
		}
		ctx.Infof("exported %d proxies (%s)", n, dbOpts.format)
		return 0
	})
}

func runDBVacuum(ctx *cli.Context, _ []string) int {
	return withDB(ctx, func(d *db.DB) int {
		b1, w1 := db.Size(d.Path())
		start := time.Now()
		if err := d.Vacuum(ctx); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		_ = d.SetMeta(ctx, db.MetaLastVacuum, fmt.Sprint(time.Now().UnixMilli()))
		b2, w2 := db.Size(d.Path())
		fmt.Fprintf(ctx.Stdout, "vacuumed %s: %s -> %s (%s)\n", paths.Pretty(d.Path()),
			humanBytes(b1+w1), humanBytes(b2+w2), roundDur(time.Since(start)))
		return 0
	})
}

func runDBRebuild(ctx *cli.Context, _ []string) int {
	return withDB(ctx, func(d *db.DB) int {
		res, err := d.RebuildStats(ctx)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		_ = d.SetMeta(ctx, db.MetaLastRebuild, fmt.Sprint(time.Now().UnixMilli()))
		if dbOpts.json {
			return ctx.JSON(res)
		}
		fmt.Fprintf(ctx.Stdout, "rebuilt the summaries of %d proxies from %d measurements (%s)\n",
			res.Proxies, res.Measurements, roundDur(res.Elapsed))
		return 0
	})
}

func runDBMigrate(ctx *cli.Context, _ []string) int {
	path := ctx.Config.DBFile()
	_, statErr := os.Stat(path)
	if dbOpts.status && statErr != nil {
		ctx.Errorf("no database at %s yet", paths.Pretty(path))
		return 1
	}
	if !dbOpts.status {
		if err := paths.Ensure(); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
	}
	d, err := db.OpenWith(path, db.Options{NoMigrate: true, BusyTimeout: ctx.Config.DB.BusyTimeout.D()})
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	defer d.Close()
	applied := 0
	if !dbOpts.status {
		if applied, err = d.Migrate(ctx); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
	}
	infos, err := d.Migrations(ctx)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	if dbOpts.json {
		return ctx.JSON(struct {
			Applied    int                `json:"applied_now"`
			Migrations []db.MigrationInfo `json:"migrations"`
		}{applied, infos})
	}
	c := ctx.Colors()
	for _, m := range infos {
		mark, when := c.Yellow+"pending"+c.Reset, ""
		if m.Applied {
			mark = c.Green + "applied" + c.Reset
			when = c.Gray + time.UnixMilli(m.AppliedAt).Format("2006-01-02 15:04") + c.Reset
		}
		fmt.Fprintf(ctx.Stdout, "  %s%3d%s  %-30s %s  %s\n", c.Cyan, m.Version, c.Reset, m.Name, mark, when)
	}
	if !dbOpts.status {
		ctx.Infof("%d migration(s) applied; schema at v%d", applied, db.LatestVersion())
	}
	return 0
}

func runDBReset(ctx *cli.Context, _ []string) int {
	path := ctx.Config.DBFile()
	if !dbOpts.yes {
		if f, ok := ctx.Stdin.(*os.File); !ok || !ui.IsTerminal(f) {
			ctx.Errorf("refusing to reset %s without --yes (stdin is not a terminal)", paths.Pretty(path))
			return 1
		}
		if !confirm(ctx.Stdin, ctx.Stderr, fmt.Sprintf("Delete %s and start over?", paths.Pretty(path))) {
			ctx.Infof("aborted")
			return 1
		}
	}
	backup := ctx.Config.DB.Backup && !dbOpts.noBackup
	if _, err := os.Stat(path); err == nil && backup {
		d, err := db.OpenWith(path, db.Options{NoMigrate: true})
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		bak := path + ".bak-" + time.Now().Format("20060102-150405")
		err = d.Backup(context.WithoutCancel(ctx), bak)
		d.Close()
		if err != nil {
			ctx.Errorf("backup failed, nothing deleted: %v", err)
			return 1
		}
		ctx.Infof("backup: %s", paths.Pretty(bak))
	}
	if err := db.Remove(path); err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	if dbOpts.empty {
		d, err := db.OpenWith(path, db.Options{BusyTimeout: ctx.Config.DB.BusyTimeout.D()})
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		d.Close()
	} else {
		d, err := openDB(ctx)
		if err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		d.Close()
	}
	ctx.Infof("reset %s", paths.Pretty(path))
	return 0
}

var errNoAlive = errors.New("the database has no alive proxies (run build first)")
