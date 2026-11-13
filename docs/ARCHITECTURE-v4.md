# SilkRouter v4 — architecture contract

This file is the shared contract for the v4 upgrade. Every change must stay
consistent with it; if a task needs to deviate, update this file in the same
commit and say why.

## Goals

1. All persistent state under one directory (`~/.config/szmelc/proxy/`).
2. `px`, `s-proxy` and `silkrouter` are the same binary and behave identically.
3. An embedded SQLite database holds every known proxy (~13k today) and
   everything we ever learned about it.
4. A `full` scan mode that measures every address with every available check
   and ranks them fastest / cleanest. Resumable and gentle.
5. A complete usage log of every proxied command, with per-proxy counters.
6. Heuristic tags: stable, fast, favourite-for-<command>, recommended lists.
7. Session / process-tree proxying from the shell, with a zsh/p10k indicator.
8. A Go TUI to browse and manage all of it.
9. Packaging: PKGBUILD, flake.nix, completions, docs.

## Paths (`internal/paths`)

`Home()` resolves, in order:

1. `$SILKROUTER_HOME` (used by tests — tests must never touch the real dir),
2. `${XDG_CONFIG_HOME:-$HOME/.config}/szmelc/proxy`.

Layout inside Home:

| Path              | Content                                              |
|-------------------|------------------------------------------------------|
| `config.toml`     | user configuration (see below)                       |
| `silkrouter.db`   | SQLite database (WAL mode, `-wal`/`-shm` beside it)  |
| `lists/`          | downloaded reputation / known-proxy lists (cached)   |
| `logs/`           | plain-text logs (scan runs, daemon)                  |
| `shell/`          | generated shell init snippets                        |
| `run/`            | runtime state (session files, relay sockets, pids)   |
| `cache.list`      | legacy text cache export (`scheme://host:port [Nms]`)|

`$PX_LIST` / `-f FILE` keep working as a legacy text-cache override (so does
the `cache.file` config key): with one of them route/build/recache/list work
on that text file exactly as before (build/recache still record into the DB
when `db.record` is on). Otherwise they use the database (`route.pool =
cache` makes route pick from `cache.list` instead; `cache.mirror = true`
keeps `cache.list` rewritten by build/recache in DB mode). The predicate is
`textCacheMode(ctx)` in `cmd_db.go`: `-f` given, or `Origin("cache.file")`
not `default`.
On first run, an existing legacy cache (`$PX_LIST` or
`<tmp>/silkrouter-proxy.list`, whichever first holds a valid proxy) is copied
to `cache.list` if that file does not exist yet (`cache.ImportLegacy`, run by
the first-run hook in `main.go`), and the db layer imports `cache.list` into
the DB. px.sh's generic `<tmp>/proxy.list` is not auto-imported; point
`$PX_LIST` at it.

`paths.Ensure()` creates Home and its sub-directories (mode 0700). It runs only
for commands registered with `NeedsHome: true`; read-only commands (help,
completion, `paths`, `config get/show`, ...) never create anything.

## Configuration (`internal/config`)

- Format: **TOML** (`github.com/BurntSushi/toml`), file `config.toml`.
- Precedence: built-in defaults < `config.toml` < environment
  (`SILKROUTER_*`, legacy `PX_*`) < command-line flags.
- `px config {path,show,get,set,edit,init,reset,keys}`; `edit` uses
  `$VISUAL`/`$EDITOR`. `set`/`reset`/`init` rewrite the file in canonical,
  commented form (keys equal to their default are commented out) and keep
  `config.toml.bak`. `show --sources` / `Config.Origin(key)` report where a
  value came from (`default`, `file`, `env NAME`).
- Every tunable introduced by any task (timeouts, worker counts, tag
  thresholds, prompt icon, …) gets a config key with a documented default.
- Adding a key = one field in the section struct in `config.go` plus its
  default in `Default()`. Struct tags: `toml:"name"` (key), `help:"…"`
  (required, shown in the file and `config keys`), optional `choices:"a|b"`,
  `min:"N"`, `env:"LEGACY_NAME"`. Get/set, env (`SILKROUTER_<TABLE>_<KEY>`),
  validation, the commented file, `config keys` and completion all derive
  from that. Supported types: string, bool, ints, floats, `[]string`
  (comma-separated on the command line) and `config.Duration` (seconds or a
  Go duration; use it for every time value). Nested structs are tables.
- Sections declared for later tasks (empty today): `[scan]`, `[tags]`,
  `[history]`, `[shell]`, `[tui]`. Existing: `[cache]`, `[source]`,
  `[probe]`, `[route]`, `[build]`, `[check]`, `[blcheck]`, `[proxycheck]`,
  `[ui]`, `[db]` (`path`, `auto_import`, `batch_size`, `flush_interval`,
  `busy_timeout`, `record`, `backup`, `list_limit`, `show_recent`; see
  `config keys`). `Config.DBFile()` resolves `db.path` (default
  `paths.DB()`); always use it instead of `paths.DB()`.
- `config.Load(path, os.LookupEnv)` = defaults < file < env (what commands
  get in `ctx.Config`); `config.LoadFile(path)` skips the env layer and is what
  anything that writes the file must use. A missing file is not an error;
  unknown keys and invalid values become warnings (invalid values fall back
  to the default).

## CLI (`internal/cli`)

- Parser: standard library `flag`. Both `-flag` and `--flag` must work, and
  existing single-dash long flags (`-builtin`, `-probe`, `-test-url`) must keep
  parsing. No cobra/pflag.
- Commands live in their own `cmd_*.go` files and register themselves with
  `cli.Register(cli.Command{...})` from `init()`. `main.go` only boots the
  registry — tasks must not need to edit a central switch.
- A `Command` has: name, aliases, group, one-line summary, long description,
  usage line, flag set, examples, and a `Run(ctx, args) int`. Help and shell
  completion are generated from this metadata. Concretely:

  ```go
  cli.Register(cli.Command{
      Name: "list", Aliases: []string{"ls"}, Group: cli.GroupCache,
      Summary: "print a report of the current cache",
      Description: "…", Usage: "[options] <arg>",
      Flags: func(fs *cli.FlagSet) {       // declare only; runs for help too
          fs.String(&opts.cache, "f,cache", fs.Config.CacheFile(), "proxy cache file").
              Placeholder("FILE").Files().ConfigKey("cache.file")
          fs.Bool(&opts.json, "json", false, "print JSON")
      },
      Args:      cli.ArgsFiles,             // positional completion
      Examples:  []cli.Example{{Cmd: "list --json", Desc: "…"}},
      NeedsHome: true,                      // Ensure() + first-run hook first
      Run: func(ctx *cli.Context, args []string) int { … },
  })
  ```

  `*cli.Context` embeds the signal-cancelled `context.Context` and carries
  `Config`, `ConfigPath`, `Flags` (`Changed(name)`), `Stdout/Stderr/Stdin`,
  `Quiet`, `Verbose`, `Prog`, `Legacy`, plus `Errorf/Infof/Debugf`,
  `UsageError` (exit 2), `JSON(v)`, `Colors()`. Flag names are
  comma-separated (`"j,workers,jobs"`); `fs.Duration` takes
  `config.Duration`. Other fields: `Subcommands []cli.Command` (nested
  groups, e.g. `config set`), `PassThrough` (stop parsing at the first
  positional — route; other commands accept flags after positionals),
  `Hidden`. Groups: `cli.GroupRoute`, `GroupCache`, `GroupDB`, `GroupScan`,
  `GroupCheck`, `GroupHistory`, `GroupShell`, `GroupTUI`, `GroupConfig`,
  `GroupMeta` (help lists them in that order).
- Dynamic completion values come from `cli.RegisterCompleter(topic, fn)`
  and are served by the hidden `px __complete <topic>` (topics today:
  `commands`, `probes`, `config-keys`); use them with `Flag.Dynamic(topic)`
  or `cli.ArgsDynamic(topic)`.
- Global flags (before the command): `--config FILE`, `--home DIR`,
  `--no-color`, `-q/--quiet`, `-v/--verbose`, `--version`, `-h/--help`;
  `--no-color`, `-q`, `-v` also work after the command unless it defines the
  same name (route's `-v` prints the exit IP, as in px.sh). Usage errors exit
  2.
- Help output is colored and aligned (groups, flags table, examples);
  `--no-color` and `NO_COLOR` disable color. Data commands offer `--json`.
- `px completion {zsh,bash,fish}` prints a completion script generated from the
  registry. `make completions` writes them to `completions/` (generated,
  gitignored); `make install` installs them for all three names under
  `$(PREFIX)/share/{zsh/site-functions,bash-completion/completions,fish/vendor_completions.d}`.
- argv[0] dispatch: `px`, `s-proxy`, `silkrouter` (and `.exe` variants) are
  identical; argv[0] only changes the name help and messages print.
  `make install` installs `silkrouter` plus `px`/`s-proxy` symlinks into
  `$(PREFIX)/bin` (`PREFIX ?= $(HOME)/.local`, `DESTDIR` supported).
- Legacy `px.sh` style keeps working: if the first argument is not a known
  subcommand, the invocation is treated as `route` (so `px curl ifconfig.me`
  and `px -d 500 -s curl …` work). `px -- <cmd>` always routes, which is how a
  command whose name matches a subcommand is run. px.sh mode flags in first
  position map to commands: `-B`/`--build` = build, `-R`/`--recache` =
  recache, `-L`/`--list` = list, `-F FILE`/`--file FILE` = `build -F FILE`,
  `--fresh` = `route -r`.

## Database (`internal/db`)

- Driver: `modernc.org/sqlite` (pure Go; `CGO_ENABLED=0` and cross-compiling
  must keep working). It requires Go >= 1.26 (go.mod says `go 1.26.0`).
- Pragmas (in the DSN, so every pooled connection has them):
  `journal_mode=WAL`, `busy_timeout=5000` (`db.busy_timeout`),
  `foreign_keys=ON`, `synchronous=NORMAL`; write transactions use
  `BEGIN IMMEDIATE` (`_txlock=immediate`) so concurrent processes queue on
  the busy timeout instead of failing a read->write upgrade. New files are
  created 0600.
- Concurrency: bulk writers (scans) send results to **one writer goroutine**
  (`db.Writer`) that commits in batches. Short-lived processes (a single
  `route`) use one small transaction. Never open one write transaction per scan
  worker. Opening an up-to-date database takes no write lock (route stays
  fast while a scan writes).
- Migrations: `schema_version(version, name, applied_at)` + ordered Go
  migrations (`migrations.go`), each applied in its own transaction together
  with its version row. Later tasks add migrations by appending
  `{Version: N+1, Name: "...", Up: ...}` to `migrations` (`execAll(stmts...)`
  for plain DDL); they never edit old ones. `Open` applies pending ones;
  `px db migrate [--status]` shows / applies them.
- Fact tables are the source of truth. Summary columns on `proxies` are
  denormalized for fast sorting and updated in the same transaction as the
  fact insert. `px db rebuild-stats` recomputes them from the facts. One Go
  function (`summary.fold`) defines how a measurement changes the summary;
  both paths use it and fold facts in insertion (`id`) order, so a rebuild
  reproduces the incremental state exactly.
- Timestamps are INTEGER unix milliseconds (UTC); Go models use `time.Time`
  (zero = NULL).
- The first open of an empty database (`db.auto_import`, done by
  `openDB(ctx)` in `cmd_db.go`) imports the embedded list (source
  `builtin`) and `<home>/cache.list` (source `cache`; every entry alive, its
  latency a `probe` measurement with probe `legacy`) in one immediate
  transaction (`DB.FirstRun`).

Core schema (migration 1; later tasks extend it). Additions to the original
sketch, made in migration 1: `proxies.username/password` (authenticated
proxies keep their credentials when route picks from the DB) and
`proxies.ms_count/ms_sum` (timed successes and their sum, so `avg_ms =
ms_sum / ms_count` is exact and reproducible).

```
proxies      id PK, scheme, host (lower-case), port, UNIQUE(scheme,host,port),
             username, password, source, added_at, last_seen_in_source,
             -- summary columns:
             status ('unknown'|'alive'|'dead'), checked_at,
             last_ms, best_ms, avg_ms (REAL), ms_count, ms_sum,
             ok_count, fail_count, ok_streak, fail_streak, exit_ip,
             country, asn, org,
             anonymity ('unknown'|'transparent'|'anonymous'|'elite'),
             listed_count, speed_score, clean_score, rank
             -- indexes: (status,last_ms), rank, last_ms, host, checked_at, exit_ip
measurements id PK, proxy_id FK (cascade), ts, kind
             ('probe'|'preflight'|'route'|'scan'),
             probe, ok, ms, err_class, exit_ip, detail (JSON)
             -- indexes: (proxy_id,ts), ts
reputation   id PK, ip, list_name, list_kind
             ('dnsbl'|'ipset'|'proxylist'|'torexit'|'datacenter'|'api'),
             listed, detail, checked_at, UNIQUE(ip,list_name)
             -- index: (ip,listed)
usage_log    id PK, ts_start, ts_end, proxy_id FK NULL (set null), proxy_url,
             mode ('route'|'shell'|'relay'), argv (JSON), cmd (basename),
             cwd, tty, pid, ppid, session_id,
             preflight_ok NULL, preflight_ms NULL,
             exit_code NULL, duration_ms, err_class
             -- indexes: (proxy_id,ts_start), (cmd,ts_start), ts_start, session_id
tags         proxy_id FK (cascade), tag, scope ('' or e.g. a command name),
             since, reason, PRIMARY KEY(proxy_id, tag, scope)
             -- index: (tag,scope)
scan_runs    id PK, mode, started_at, finished_at, total, done, cursor,
             opts (JSON), state ('running'|'paused'|'done'|'failed')
             -- index: (mode,state)
meta         key PK, value
```

Summary semantics (`summary.fold`): status follows the latest *inserted*
fact (id order; ok -> alive, failure -> dead), so a producer that writes an
older-timestamped fact after a newer one flips status to the older verdict;
`checked_at` = newest fact ts regardless of order; `last_ms`,
`best_ms`, `ms_count`, `ms_sum`, `avg_ms` consider timed successes only (a
failure leaves them alone); a success increments `ok_count`/`ok_streak` and
zeroes `fail_streak`, and vice versa; `exit_ip` = latest non-empty one.
`listed_count` = distinct lists with `listed = 1` for the proxy's host or
exit IP, refreshed whenever reputation rows for that IP are written.

### Go API (what later tasks build on)

```go
db.Open(path) / db.OpenWith(path, db.Options{BusyTimeout, NoMigrate, ReadOnly})
d.Close(); d.Path(); d.SQL(); d.Tx(ctx, func(*sql.Tx) error)
d.Migrate(ctx); d.SchemaVersion(ctx); d.Migrations(ctx); db.LatestVersion()
d.Vacuum(ctx); d.Backup(ctx, dst); db.Size(path); db.Remove(path)

// proxies
d.UpsertProxy(ctx, db.Addr, source) (id, error)
d.ImportProxies(ctx, []proxy.Proxy, db.ImportOptions{Source, Latencies, AllAlive, Probe}) (db.ImportResult, error)
d.FirstRun(ctx, db.FirstRunOptions{Builtin, CacheFile}) (db.FirstRunResult, error)
d.GetProxy(ctx, id); d.GetProxyByAddr(ctx, a); d.GetProxyByURL(ctx, s)
d.FindProxy(ctx, "42" | "#42" | URL | host[:port]); d.DeleteProxy(ctx, id)
d.ListProxies(ctx, db.Filter) ([]*db.Proxy, error); d.CountProxies(ctx, db.Filter)
d.SetScores(ctx, []db.SetScore{ProxyID, SpeedScore, CleanScore, Rank})  // T3 ranking
d.SetGeo(ctx, id, country, asn, org, anonymity)
db.Filter{IDs, Status, Schemes, MinMS, MaxMS, IncludeUntimed, Tag, TagScope,
          Countries, ListedMax *int, MinOKStreak, Source, Search, CheckedSince,
          Sort, Reverse, Limit, Offset, WithTags}
db.SortKeys(); db.ExpandSchemes([]string{"socks"}, httpGroup)
db.AddrOf(proxy.Proxy); db.ParseAddr(s); (*db.Proxy).ToProxy()/URL()/Key()

// facts
d.RecordMeasurement(ctx, db.Measurement{ProxyID | Addr, TS, Kind, Probe, OK, MS, ErrClass, ExitIP, Detail})
d.RecordMeasurements(ctx, []db.Measurement); d.Measurements(ctx, proxyID, limit)
d.RebuildStats(ctx) (db.RebuildResult, error); d.Stats(ctx) (*db.Stats, error)
d.UpsertReputation(ctx, ...db.Reputation); d.ReputationFor(ctx, ips...)
d.StartUsage(ctx, db.Usage) (id, error); d.SetUsagePreflight(ctx, id, ok, ms)
d.FinishUsage(ctx, id, exitCode, errClass, end); d.ListUsage(ctx, db.UsageFilter); d.UsageCount(ctx, proxyID)
d.SetTag/SetTags(ctx, db.Tag{ProxyID, Tag, Scope, Since, Reason}); d.ClearTag(ctx, id, tag, scope)
d.ClearTags(ctx, tag, scope|"*"); d.ReplaceTags(ctx, tagNames, []db.Tag); d.Tags(ctx, id); d.TagCounts(ctx)
d.CreateScanRun(ctx, db.ScanRun); d.UpdateScanRun(ctx, id, done, cursor, state, total)
d.GetScanRun(ctx, id); d.ResumableScanRun(ctx, mode); d.ListScanRuns(ctx, limit)
d.GetMeta/SetMeta(ctx, key, value); d.AllMeta(ctx)
d.Export(ctx, w, db.FormatList|FormatURLs|FormatCSV|FormatJSON, db.Filter)

// bulk writer (scans)
w := d.NewWriter(ctx, db.WriterOptions{BatchSize, FlushInterval, OnError})
w.Measure(db.Measurement); w.Reputation(db.Reputation); w.Flush(); w.Close(); w.Stats(); w.Done()
```

The Writer's channel is unbuffered: a nil error from `Measure` means the
writer owns the row. On `Close` or cancellation of its context it commits
what is pending with a non-cancelled context (Ctrl-C keeps gathered data);
later sends return `db.ErrWriterClosed`. A failed batch is reported through
`OnError` and `Close`'s error and dropped. Measurements with `ProxyID == 0`
create their proxy from `Addr`.

`probe.ClassifyErr(err)` maps dial/probe errors to the err_class vocabulary;
`cache.BuildOptions.OnResult` and `router.Options.{Pool, PoolLabel,
OnVerify, OnChosen}` are the hooks the CLI uses to feed / read the DB.
CLI helpers in package main: `openDB(ctx)` (open + auto-import), `withDB`,
`newRecorder`, `textCacheMode`, `filterFlags`/`filterOpts.build`
(shared filter flags), `printProxyTable`.

Commands (group `cli.GroupDB`): `px db {info,path,import,export,vacuum,
rebuild-stats,migrate,reset}`, `px proxies` (alias `db-ls`), `px show`
(alias `inspect`). `db path`, `db info` and `db migrate --status` never
create the file.

`err_class` vocabulary: `timeout`, `refused`, `reset`, `dns`, `tls`, `auth`,
`proto`, `http_status`, `exit_nonzero`, `other`.

## Success semantics (usage log)

A route records two things separately: the **preflight** (did the proxy
answer the check before running the command, and how fast) and the **command
exit code**. "Proxy success" for streaks and tags means the preflight or the
measured connection succeeded; a non-zero exit is counted per command, but
does not by itself mark the proxy as broken.

## Heuristic tags (`internal/score`)

Defaults (all configurable under `[tags]`):

- `stable`: last 10 uses/checks all succeeded.
- `fast`: last 10 successful measurements all under 100 ms.
- `fav:<cmd>`: used at least 10 times for `<cmd>` with a success rate above
  the threshold.
- `clean`: listed on 0 reputation lists after a full check.
- `recommended:<cmd>`: best combined score for a command.

## Shell integration (`internal/shell`)

- A child cannot change its parent's environment, so the entry point is
  `eval "$(px shell-init zsh|bash)"`, which defines the `px on` / `px off`
  shell functions wrapping the binary.
- On-mode exports the proxy env vars plus `SILKROUTER_ACTIVE`,
  `SILKROUTER_PROXY`, `SILKROUTER_SESSION`. Commands run while on are logged
  through a `preexec`/`precmd` hook (mode `shell`).
- The p10k segment reads environment variables only — no subprocess — so
  instant prompt keeps working.
- `px shell install` writes a marked block (with a backup) to rc files; it is
  never run by tests and never edits real dotfiles during development.

## TUI (`internal/tui`)

bubbletea + lipgloss + bubbles + glamour, modeled on
`~/Projects-4/ULTIMATE-TUI` (another module: copy patterns, do not import).

## Testing rules

- Unit tests use `t.TempDir()` and set `SILKROUTER_HOME`. Tests that run a
  `NeedsHome` command also set `TMPDIR` to a temp dir and clear `PX_LIST`, so
  the first-run import never reads a real legacy cache (see `sandbox()` in
  `main_test.go`).
- No real network in unit tests except local fake servers / fake proxies.
- Never run a real full scan of the whole list as a test; use small samples.
- Never touch `~/.config/szmelc`, `~/.zshrc`, `~/.p10k.zsh` from tests or
  development runs.
- `make check` (fmt-check, vet, test) must pass before every commit.
