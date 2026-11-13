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

`$PX_LIST` / `-f FILE` keep working as a legacy text-cache override. On first
run, an existing legacy cache (`$PX_LIST` or `<tmp>/silkrouter-proxy.list`) is
imported into the DB.

## Configuration (`internal/config`)

- Format: **TOML** (`github.com/BurntSushi/toml`), file `config.toml`.
- Precedence: built-in defaults < `config.toml` < environment
  (`SILKROUTER_*`, legacy `PX_*`) < command-line flags.
- `px config {path,show,get,set,edit,init,reset}`; `edit` uses `$EDITOR`.
- Every tunable introduced by any task (timeouts, worker counts, tag
  thresholds, prompt icon, …) gets a config key with a documented default.

## CLI (`internal/cli`)

- Parser: standard library `flag`. Both `-flag` and `--flag` must work, and
  existing single-dash long flags (`-builtin`, `-probe`, `-test-url`) must keep
  parsing. No cobra/pflag.
- Commands live in their own `cmd_*.go` files and register themselves with
  `cli.Register(cli.Command{...})` from `init()`. `main.go` only boots the
  registry — tasks must not need to edit a central switch.
- A `Command` has: name, aliases, group, one-line summary, long description,
  usage line, flag set, examples, and a `Run(ctx, args) int`. Help and shell
  completion are generated from this metadata.
- Help output is colored and aligned (groups, flags table, examples);
  `--no-color` and `NO_COLOR` disable color. Data commands offer `--json`.
- `px completion {zsh,bash,fish}` prints a completion script generated from the
  registry.
- argv[0] dispatch: `px`, `s-proxy`, `silkrouter` (and `.exe` variants) are
  identical.
- Legacy `px.sh` style keeps working: if the first argument is not a known
  subcommand, the invocation is treated as `route` (so `px curl ifconfig.me`
  and `px -d 500 -s curl …` work). `px -- <cmd>` always routes, which is how a
  command whose name matches a subcommand is run.

## Database (`internal/db`)

- Driver: `modernc.org/sqlite` (pure Go; `CGO_ENABLED=0` and cross-compiling
  must keep working).
- Pragmas: `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON`,
  `synchronous=NORMAL`.
- Concurrency: bulk writers (scans) send results to **one writer goroutine**
  that commits in batches. Short-lived processes (a single `route`) use one
  small transaction. Never open one write transaction per scan worker.
- Migrations: `schema_version` table + ordered Go migrations
  (`migrations.go`). Later tasks add migrations; they never edit old ones.
- Fact tables are the source of truth. Summary columns on `proxies` are
  denormalized for fast sorting and updated in the same transaction as the
  fact insert. `px db rebuild-stats` recomputes them from the facts.

Core schema (migration 1; later tasks extend it):

```
proxies      id PK, scheme, host, port, UNIQUE(scheme,host,port),
             source, added_at, last_seen_in_source,
             -- summary columns:
             status ('unknown'|'alive'|'dead'), checked_at,
             last_ms, best_ms, avg_ms, ok_count, fail_count,
             ok_streak, fail_streak, exit_ip, country, asn, org,
             anonymity ('unknown'|'transparent'|'anonymous'|'elite'),
             listed_count, speed_score, clean_score, rank
measurements id PK, proxy_id FK, ts, kind ('probe'|'preflight'|'route'|'scan'),
             probe, ok, ms, err_class, exit_ip, detail (JSON)
reputation   id PK, ip, list_name, list_kind
             ('dnsbl'|'ipset'|'proxylist'|'torexit'|'datacenter'|'api'),
             listed, detail, checked_at, UNIQUE(ip,list_name)
usage_log    id PK, ts_start, ts_end, proxy_id FK NULL, proxy_url,
             mode ('route'|'shell'|'relay'), argv (JSON), cmd (basename),
             cwd, tty, pid, ppid, session_id,
             preflight_ok NULL, preflight_ms NULL,
             exit_code NULL, duration_ms, err_class
tags         proxy_id FK, tag, scope ('' or e.g. a command name),
             since, reason, PRIMARY KEY(proxy_id, tag, scope)
scan_runs    id PK, mode, started_at, finished_at, total, done, cursor,
             opts (JSON), state ('running'|'paused'|'done'|'failed')
meta         key PK, value
```

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

- Unit tests use `t.TempDir()` and set `SILKROUTER_HOME`.
- No real network in unit tests except local fake servers / fake proxies.
- Never run a real full scan of the whole list as a test; use small samples.
- Never touch `~/.config/szmelc`, `~/.zshrc`, `~/.p10k.zsh` from tests or
  development runs.
- `make check` (fmt-check, vet, test) must pass before every commit.
