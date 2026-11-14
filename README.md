# SilkRouter

A single, cross-platform **Go binary** with no runtime dependencies that
keeps a SQLite database of every known proxy, scans and ranks them, routes
any command (or a whole shell) through the best one, and logs every use.

SilkRouter is a full rewrite of the original bash toolkit (`px.sh`,
`blcheck.sh`, `proxycheck.sh`) into one static binary. No `curl`, no `awk`, no
`shuf`: nothing to install. It cross-compiles to Linux, Windows, macOS and
FreeBSD with `CGO_ENABLED=0`. The same binary answers to **`px`**,
**`s-proxy`** and **`silkrouter`**, and the old `px.sh` invocation style keeps
working (`px curl ifconfig.me`, `px -d 500 -s git pull`, `px -B`).

> The original shell scripts are preserved under [`legacy/`](legacy/).

---

## Highlights

- **One binary, three names, all tools.** `px`, `s-proxy` and `silkrouter`
  are identical; every feature is a subcommand with colored, grouped help.
- **Everything in `~/.config/szmelc/proxy/`.** Config, database, downloaded
  IP lists, logs, shell snippets and session state live in one directory.
- **A database of every proxy.** An embedded, pure-Go SQLite database holds
  all ~12k known proxies and every fact ever learned about them:
  measurements, reputation verdicts, usage, tags, scan runs.
- **Full scans: know everything before you tunnel.** `px scan full` checks
  every address with every probe (median latency, jitter), finds its exit IP,
  tests HTTPS through it, grades anonymity (transparent / anonymous / elite),
  detects which protocols the port speaks, and looks the host and exit IP up
  on known-proxy, Tor, VPN, datacenter and abuse lists plus 15 DNS blacklists
  and a geolocation / ASN service. Gentle, resumable (Ctrl-C pauses).
- **Ranking.** Speed, clean and stability scores (0-100) and a rank for every
  alive proxy: balanced, or fastest / cleanest / steadiest first.
- **Usage log.** Every proxied command is recorded (when, which proxy, the
  command line, cwd, tty, session, exit code, duration, the proxy latency
  measured while it ran), with per-proxy counters, error classes (curl 7 =
  refused, 28 = timeout, ...) and `px stats` per proxy / command / day.
- **Heuristic tags.** `stable` (10 successes in a row), `fast` (10 timed
  successes under 100 ms), `fav:<cmd>` (10+ good uses for a command),
  `recommended[:<cmd>]`, `clean`, `elite`, `https`, `flaky`, `tor`,
  `datacenter`, `known`; plus your own (`blocked` keeps a proxy out of route).
- **Smart routing.** `px <cmd>` prefers the proxies recommended for that
  command; `--best`, `--tag fast,elite`, `--for git`, `--chain` (proxychains:
  every connection of the process tree).
- **Shell integration.** `px on` proxies your current shell and everything
  started from it, `px rotate` switches, `px off` restores; `px shell` starts
  a proxied subshell. Commands typed meanwhile are logged, and the prompt
  shows a segment (powerlevel10k `silkrouter`, or a prefix on plain zsh /
  bash / fish prompts).
- **TUI.** `px tui`: dashboard, proxies, history, tags, scans, lists and
  config views with a `:` command palette (see [docs/TUI.md](docs/TUI.md)).
- **All protocols, natively.** `http`, `https`, `socks4`, `socks4a` and
  `socks5` are dialed by standard-library clients.
- **Rich CLI.** `-x` and `--long` flags, `--json` for data commands, config
  keys for every tunable, generated zsh / bash / fish completion.
- **Packaged.** `make` (self-documenting), Arch `PKGBUILD`, Nix flake
  (package, app, dev shell, overlay, home-manager module).

---

## Install

### From source (Go 1.26+)

```sh
git clone https://github.com/Szmelc-INC/SilkRouter
cd SilkRouter
make                  # lists every target
make install          # no root needed: ~/.local/bin + completions + shell snippets
silkrouter shell install
```

`make install` puts `silkrouter` plus the `px` and `s-proxy` symlinks into
`~/.local/bin`, completions for all three names under `~/.local/share` (zsh,
bash, fish), the shell integration scripts and an example config under
`~/.local/share/silkrouter`. Other locations:

```sh
make install PREFIX=/opt/silkrouter            # any prefix you can write to
make install PREFIX=/usr DESTDIR="$pkgdir"     # packaging (staged install)
make uninstall                                 # same PREFIX/DESTDIR rules
```

The binary has **no runtime dependencies** (SQLite is compiled in through the
pure-Go `modernc.org/sqlite`). `make release` cross-compiles every platform
into `dist/` with `SHA256SUMS`.

### Arch Linux

```sh
cd packaging/arch && makepkg -si      # or: make pkg-arch
```

[`packaging/arch/PKGBUILD`](packaging/arch/PKGBUILD) builds `silkrouter-git`
(static, PIE on x86_64 / aarch64) with completions, shell snippets and docs.
Optional: `proxychains-ng` for `--chain`, `zsh-theme-powerlevel10k`.

### Nix

```sh
nix run github:Szmelc-INC/SilkRouter/feat/v4 -- --version
nix profile install github:Szmelc-INC/SilkRouter/feat/v4
nix develop            # go, gopls, golangci-lint, make, sqlite, proxychains
```

The flake provides `packages.default`, `apps.{default,px}`,
`devShells.default`, `overlays.default` and a home-manager module:

```nix
imports = [ silkrouter.homeManagerModules.default ];
programs.silkrouter = {
  enable = true;                       # + zsh / bash / fish integration
  settings.route.max_ms = 500;         # -> ~/.config/szmelc/proxy/config.toml
};
```

After changing `go.mod`, refresh `vendorHash` in
[`packaging/nix/package.nix`](packaging/nix/package.nix) with `make nix-hash`.

### Shell completion

`make install` and the packages install completion for all three names. By
hand: `source <(px completion zsh)`, `source <(px completion bash)`,
`px completion fish | source`.

---

## Quick start

```sh
px scan quick                        # test every proxy once (alive / dead, latency)
px scan full --status alive          # everything about the working ones, then rank
px proxies --sort rank -n 20         # the best 20: latency, scores, country, lists, tags
px show 42                           # everything known about one proxy

px curl -s https://ifconfig.me       # route one command (recommended proxy first)
px --best --tag elite yt-dlp URL     # best ranked elite proxy that answers

px on                                # proxy this shell (needs: px shell install)
px history                           # what ran through which proxy
px stats --by proxy,cmd -s 7d        # usage per proxy and command this week
px off

px tui                               # browse and manage it all
```

---

## Commands

```
px [global options] <command> [options] [args]
px [route options] <program> [args...]     # legacy px.sh style: same as route
px -- <program> [args...]                  # always route
px -B | -R | -L | -F FILE                  # px.sh modes: build, recache, list, build -F

Routing         route, run       run a command through a cached/random proxy
Cache           build            build/append the proxy cache (test & sort proxies)
                recache          re-test the proxies already in the cache (prune dead)
                list, ls         report of the working proxies (database or text cache)
Database        db               manage the SQLite database of every known proxy
                proxies, db-ls   list proxies (filter, sort, page)
                show, inspect    everything known about one proxy
Scanning        scan             quick | full | reputation | geo | protocols | runs
                rank             recompute speed / clean / stability scores and ranks
                lists            known-proxy / Tor / VPN / datacenter / abuse IP lists
Checks          check            test a single proxy or a file of proxies
                blcheck          check IP(s) against ~60 DNS blacklists (mxtoolbox)
                proxycheck       proxy / anonymity detection (whatismyipaddress)
                probes           list the available probes
History & tags  history, log     the usage log of every proxied command
                stats            usage statistics per proxy / command / error / time
                tags, tag        heuristic and manual tags (refresh, add, rm, show)
                favorites, favs  favourite proxies per command (fav:<cmd>)
                recommend, rec   recommended proxies, overall or for a command
Shell           on / off         proxy this shell (and its process tree) / stop
                rotate           switch this shell's session to another proxy
                session, status  current session; list / show / end / prune
                shell            proxied subshell; shell install / uninstall
                shell-init       print the zsh / bash / fish integration script
                prompt           print the prompt segment (starship, tmux, ...)
Interface       tui, ui          interactive terminal UI
Configuration   config, cfg      show and edit config.toml
                paths, home      the config home and every file in it
Help & info     completion       shell completion script (zsh, bash, fish)
                help, version
```

`px help <command>` / `px <command> --help` shows every flag, its default and
the config key behind it. Every flag works with one or two dashes, most have a
short and a long name. Global options (before the command; `--no-color`, `-q`
and `-v` also after it): `--config FILE`, `--home DIR`, `--no-color` (or
`NO_COLOR=1`), `-q/--quiet`, `-v/--verbose`, `--version`, `-h/--help`.

`build`, `recache`, `route` and `list` work on the **database** by default.
The legacy **text cache** (`scheme://host:port [Nms]`, fastest first) is still
supported: `-f FILE`, `$PX_LIST` or `cache.file` select one, `route.pool =
"cache"` makes route use `<home>/cache.list`, and `px db export` writes it.

### `route`: run a command through a proxy

```sh
px route [options] [--] <command...>
px [options] <command...>              # same thing, px.sh style
```

Picks an **alive** proxy from the database within the filters, exports
`ALL_PROXY` / `HTTP_PROXY` / `HTTPS_PROXY` (and lowercase) and runs the
command. Pick order (`route.prefer`): **smart** tries `recommended:<cmd>`,
then `fav:<cmd>`, then `recommended`, then everything else, each group
shuffled; **rank** walks the best ranked first; **random** is the px.sh
behaviour. Proxies tagged `blocked` (`route.exclude_tags`) are skipped. The
run is recorded in the usage log, the proxy is measured while the command runs
(`history.measure_on_use`) and its tags are refreshed.

| Flag                  | Description                                     | Default (config key) |
|-----------------------|-------------------------------------------------|----------------------|
| `-d, --max-ms MS`     | Max last latency                                | `1000` (`route.max_ms`) |
| `-D, --min-ms MS`     | Min last latency                                | `0` (`route.min_ms`) |
| `-H, --http` / `-s, --socks` | Protocol filter                          | `route.scheme` |
| `-T, --tag TAGS`      | Only proxies with one of these tags             | — |
| `--exclude-tag TAGS`  | Never these tags                                | `blocked` (`route.exclude_tags`) |
| `-b, --best`          | Best ranked first (`--prefer rank`)             | — |
| `--for CMD`           | Use another command's recommendations           | the program |
| `--prefer ORDER`      | `smart`, `rank` or `random`                     | `smart` (`route.prefer`) |
| `--chain`             | Run under proxychains-ng (whole process tree)   | `route.method` |
| `-p, --proxy URL`     | Force a specific proxy                          | — |
| `-x` / `-c`           | Skip / force verification                       | `route.verify` |
| `-t, --timeout DUR`   | Verify timeout                                  | `8s` (`route.verify_timeout`) |
| `-r, --fresh`         | Grab a fresh proxy from the source              | — |
| `-v, --verbose`       | Print the exit IP before running                | — |
| `--no-log`            | Do not record this command                      | `history.record` |
| `-f, --cache FILE`    | Legacy text cache instead of the database       | `cache.file` |

### Scanning: `scan`, `rank`, `lists`

```sh
px scan full                        # every check on every proxy (resumable)
px scan full --status alive -j 64   # filters like px proxies; workers
px scan full --older-than 7d --rank clean
px scan quick                       # one probe per proxy
px scan reputation | geo | protocols
px scan runs                        # progress, state, resumable runs
px rank [-m balanced|speed|clean|stable] [--top 30]
px lists                            # list status; px lists update [--force]
px lists check 185.220.101.1        # which lists / DNS blacklists know an IP
```

`scan full` per proxy: TCP connect time; every probe in `scan.probes` ×
`scan.tries` (median, jitter; dead proxies stop after `scan.fail_fast` failed
probes); exit IP; HTTPS through the proxy (CONNECT + TLS, Cloudflare trace for
the exit country); an anonymity judge (`scan.judge_url`, compared with your
real IP); protocol detection (http / socks5 / socks4 on the same port, new
combinations are added); reputation of host and exit IP on the IP lists and
DNS blacklists; geolocation / ASN / hosting / mobile (`scan.geo`, batched,
queried directly, never through the proxy); optionally mxtoolbox
(`--mxtoolbox`) and whatismyipaddress (`--wimia`). Everything is a fact in
the database; afterwards scores, ranks and tags are recomputed. Ctrl-C pauses,
the next run with the same filters resumes.

Built-in IP lists (cached in `<home>/lists`, refreshed after
`scan.lists_ttl`): Tor exits, FireHOL proxies / anonymous / level1 / level2 /
abusers, socks-proxy, sslproxies, proxylists, proxyrss, rosinstrument, X4BNet
VPN and datacenter ranges, StopForumSpam, blocklist.de, Spamhaus DROP; add your
own with `scan.custom_lists = ["name|proxylist|https://..."]`.

Scores: **speed** = 100·ref/(ref + median + jitter/2) with `scan.speed_ref_ms`
(250) scoring 50; **clean** = 100 minus penalties for DNS blacklists, known
proxy / Tor / abuse / datacenter lists and transparent / anonymous grades;
**stable** = smoothed success rate of recent facts blended with usage. The
balanced rank weighs them by `scan.weight_speed / _clean / _stable`.

### History & tags: `history`, `stats`, `tags`, `favorites`, `recommend`

```sh
px history                               # last 50 commands (time, proxy, exit, took, ms, error)
px history -c curl -s 24h --status fail  # curl failures today
px history --session $SILKROUTER_SESSION --full
px history export --format csv -o usage.csv
px history prune --older-than 90d        # or history.retention
px stats                                 # per proxy: uses, ok, fail, proxy errors, rate, avg ms
px stats --by cmd | --by proxy,cmd | --by err | --by day | --by hour
px tags                                  # every tag and how many proxies carry it
px tags show fast | px tags add 42 blocked | px tags rm 42 blocked | px tags refresh
px favorites [cmd]                       # fav:<cmd>
px recommend [cmd] [--pick]              # ALL_PROXY=$(px rec --pick curl)
```

Rows also go to `<home>/logs/usage.log` (`history.text_log`). Every proxy
carries `use_count`, `use_ok`, `use_fail`, `use_proxy_err`, `last_used_at`
and `last_cmd` (sortable: `px proxies --sort use_count`).

| Tag | Rule (config `[tags]`) |
|-----|------------------------|
| `stable` | the last `stable_n` (10) checks / uses all succeeded |
| `fast` | the last `fast_n` (10) timed successes all under `fast_ms` (100) |
| `fav:<cmd>` | ≥ `fav_min_uses` (10) uses for `<cmd>`, ≥ `fav_min_rate` (80%) ok |
| `recommended` | the `recommend_top` (5) best ranked |
| `recommended:<cmd>` | the best for `<cmd>`: usage success, speed, cleanliness |
| `clean` / `elite` / `https` | full-scanned and on no list / no headers, IP hidden / TLS works |
| `flaky` / `tor` / `datacenter` / `known` | fails often / Tor exit / hosting range / on a known-proxy list |

### Shell integration: `on`, `off`, `rotate`, `shell`, `session`, `prompt`

A program cannot change its parent shell's environment, so the integration
defines `px` / `s-proxy` / `silkrouter` shell functions that eval what the
binary prints. Install it once:

```sh
silkrouter shell install            # marked block in ~/.zshrc (bash / fish: shell install bash)
# or by hand:  eval "$(silkrouter shell-init zsh)"   |   silkrouter shell-init fish | source
```

```sh
px on [-s] [-d 300] [--best] [--tag elite]   # this shell + every process it starts
px rotate                                     # another proxy, same session and filters
px status                                     # session, proxy, commands logged
px off                                        # restore the previous variables
px shell [--chain]                            # proxied subshell; exit ends it
px session list -a | show ID | end ID | prune
```

While a proxy is on, `preexec` / `precmd` hooks log every typed command (mode
`shell`, `shell.log_commands`), and the prompt changes:

- **powerlevel10k**: a `silkrouter` segment (icon `shell.icon`, color
  `shell.color`, text `shell.prompt_format`, e.g. `󰒍 socks5 1.2.3.4:1080
  120ms PL`) is added to your left (or right, `shell.p10k`) elements
  automatically; it only reads environment variables, so instant prompt keeps
  working. Style it with `POWERLEVEL9K_SILKROUTER_FOREGROUND`.
- **plain zsh / bash / fish**: the same text is prefixed to the prompt
  (`shell.prompt_auto`).
- **starship / tmux / anything else**: `px prompt` prints it
  (`[custom.silkrouter] command = "silkrouter prompt"`).

`--chain` (`route.method` / `shell.method = "proxychains"`) runs the command
or subshell under proxychains-ng, which forces every TCP connection of the
whole process tree through the proxy, also for programs that ignore proxy
variables.

### Interface: `tui`

`px tui` opens the interactive UI: dashboard, proxies (search, sort, filter,
detail pane, tag / block / check the selected proxy), history and stats, tags,
scan runs, lists and an inline config editor, with a `:` command palette and
a `?` help overlay. See [docs/TUI.md](docs/TUI.md).

### Database: `db`, `proxies`, `show`

```sh
px proxies                                   # 50 rows, fastest first, colored table
px proxies --status alive --socks -d 300     # fast working socks proxies
px proxies --sort rank -n 20                 # best ranked
px proxies --tag fast --anonymity elite      # (tags / filters combine)
px proxies 185.199 --json                    # search host / URL / exit IP / org
px show 42                                   # fields, scores, measurements, reputation, tags, usage

px db info | import FILE|URL|--builtin | export [--format list|urls|csv|json]
px db rebuild-stats | migrate [--status] | vacuum | reset [--yes] [--empty] | path
```

Sort keys: `rank`, `last_ms`, `best_ms`, `avg_ms`, `jitter_ms`,
`speed_score`, `clean_score`, `stable_score`, `ok_streak`, `success_rate`,
`use_count`, `use_rate`, `last_used_at`, `checked_at`, `full_scan_at`,
`anonymity`, `country`, ... (natural direction, `-r` reverses, unknown last).

### Checks: `check`, `blcheck`, `proxycheck`

```sh
px check socks5://1.2.3.4:1080          # or a file of proxies; -probe ipify shows exit IPs
px blcheck 8.8.8.8                      # mxtoolbox; -f FILE writes FILE-BL
px proxycheck socks5://1.2.3.4:1080     # whatismyipaddress; -f FILE writes FILE-PX
```

---

## Configuration

All state lives in the **config home**: `$SILKROUTER_HOME`, or
`${XDG_CONFIG_HOME:-~/.config}/szmelc/proxy` (`--home DIR` for one run).

| Path            | Content                                         |
|-----------------|-------------------------------------------------|
| `config.toml`   | user configuration                              |
| `silkrouter.db` | proxy database (SQLite, WAL)                    |
| `lists/`        | downloaded IP lists (`<name>.txt` + `.json` metadata) |
| `logs/`         | `usage.log` (one line per proxied command)       |
| `shell/`        | generated shell integration scripts             |
| `run/`          | sessions, proxychains configs, subshell bootstrap |
| `cache.list`    | legacy text cache (import / export format)      |

Precedence: defaults < `config.toml` < environment
(`SILKROUTER_<TABLE>_<KEY>`, legacy `PX_LIST`, `MXTB_TOKEN`) < flags. Durations
accept seconds (`8`) or Go durations (`8s`, `1m30s`).

```sh
px config init                     # commented default config.toml
px config set scan.workers 48      # validated; keeps config.toml.bak
px config show --sources           # effective values and where they come from
px config keys                     # every key with type, default, env variable
px config edit | get KEY | reset [KEY] | path
```

Tables: `[cache]`, `[source]`, `[probe]`, `[route]`, `[build]`, `[check]`,
`[blcheck]`, `[proxycheck]`, `[ui]`, `[db]`, `[scan]`, `[tags]`, `[history]`,
`[shell]`, `[tui]`.

---

## Caching probes

| Probe        | Endpoint                                   | Notes |
|--------------|--------------------------------------------|-------|
| `cloudflare` | `cp.cloudflare.com/generate_204`           | default, fast |
| `google`     | `www.google.com/generate_204`              | |
| `gstatic`    | `connectivitycheck.gstatic.com/generate_204` | |
| `msft`       | `www.msftconnecttest.com/connecttest.txt`  | |
| `apple`      | `captive.apple.com/hotspot-detect.html`    | |
| `example`    | `example.com`                              | IANA-reserved, very stable |
| `ipify`      | `api.ipify.org`                            | also reports exit IP |
| `aws`        | `checkip.amazonaws.com`                    | also reports exit IP |
| `icanhazip`  | `icanhazip.com`                            | also reports exit IP |

Or bring your own: `-test-url https://my.endpoint/health` (any `2xx` counts).

---

## Project layout

```
main.go             boot: names, legacy px.sh modes, first-run hook, embedded list
cmd_*.go            one file per command group, self-registered from init()
usage_track.go      usage log of routed commands (start, measure, finish, tags)
internal/
  cli/              command registry, dispatcher, help renderer, completions
  config/           typed TOML config: defaults, file, env, get/set, writer
  paths/            config home resolution and layout
  proxy/            Proxy type, parsing, http/https/socks4/socks5 dialers
  probe/            probes (registry: add methods here)
  source/           candidate lists (URL / file / embedded)
  db/               SQLite schema, migrations, typed queries, batched writer
  cache/            legacy build pipeline (worker pool), text cache
  router/           proxy selection + command execution (+ proxychains)
  scan/             full-scan engine, blcheck + proxycheck scrapers
  reputation/       IP lists, DNS blacklists, geolocation client
  score/            speed / clean / stability scores, ranks, heuristic tags
  history/          exit-code classes, tty, text log, time windows
  shell/            sessions, init scripts, rc blocks, subshell bootstrap
  tui/              interactive terminal UI (Bubble Tea)
  ui/               cross-platform colour handling
packaging/          arch/PKGBUILD (+ .SRCINFO), nix/package.nix, nix/hm-module.nix
legacy/             original bash scripts (px.sh, blcheck.sh, proxycheck.sh)
proxy/              free proxy sources + consolidated LIST.txt (embedded)
```

See [`docs/ARCHITECTURE-v4.md`](docs/ARCHITECTURE-v4.md) for the contract
(paths, config, CLI, database, scans, shell, testing).

---

## Development

```sh
make              # help: every target, grouped
make build        # host binary
make check        # fmt-check + vet + test (before every commit)
make cover        # coverage report -> build/coverage.html
make tui          # build + px tui
make install      # ~/.local (PREFIX / DESTDIR for packaging)
make release      # cross-compile -> dist/ + SHA256SUMS
make pkg-arch     # makepkg packaging/arch/PKGBUILD
make nix-build    # nix build .#default
```

---

## Notes

- Free proxy lists have high failure rates: a few hundred live proxies out of
  thousands of candidates is normal. A full scan of ~12k addresses takes a
  while on purpose (gentle); `scan quick` first, then `scan full --status
  alive`.
- Latency is a full round-trip through the proxy to the probe endpoint, not a
  ping.
- Geolocation (`scan.geo = "ip-api"`) and DNS blacklists are queried from your
  own connection; set `scan.geo = "none"` / `scan.dnsbl = false` to avoid it.
- The database only grows by facts; `px db vacuum` compacts it,
  `px history prune` / `history.retention` trims the usage log, `px db reset`
  starts over (a backup is kept).
