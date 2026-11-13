# SilkRouter

A single, cross-platform **Go binary** with no runtime dependencies that
builds a fast local proxy cache and routes any command through a random
proxy — plus built-in blacklist and proxy/anonymity checkers.

SilkRouter is a full rewrite of the original bash toolkit (`px.sh`,
`blcheck.sh`, `proxycheck.sh`) into one static binary. No `curl`, no `awk`, no
`shuf` — nothing to install. It cross-compiles to Linux, Windows, macOS (amd64
and arm64) with `CGO_ENABLED=0`. The same binary answers to **`px`**,
**`s-proxy`** and **`silkrouter`**, and the old `px.sh` invocation style keeps
working (`px curl ifconfig.me`, `px -d 500 -s git pull`, `px -B`).

> The original shell scripts are preserved under [`legacy/`](legacy/).

---

## Highlights

- **One binary, three names, all tools.** `px`, `s-proxy` and `silkrouter`
  are identical; `build`, `route`, `check`, `blcheck`, `proxycheck`, `config`
  and friends live in one subcommand-driven executable.
- **px.sh compatible.** An unknown first word is routed, so `px <command…>`
  works like it always did; `px -- <command>` routes even a program that shares
  a subcommand's name.
- **All protocols, natively.** `http`, `https`, `socks4`, `socks4a` and
  `socks5` are dialed by hand-rolled, standard-library clients — including
  SOCKS4/4a which most Go proxy libraries don't support.
- **Many ways to cache.** Validation is no longer hard-wired to Cloudflare.
  Pick from nine built-in **probes** (Cloudflare, Google, gstatic, Apple,
  Microsoft, example.com, ipify, AWS, icanhazip) or supply your own
  `-test-url`. Adding a new probe is a one-line registry entry.
- **Configurable.** Every default lives in a commented
  `~/.config/szmelc/proxy/config.toml`, overridable from the environment and
  per run with flags (`px config set route.max_ms 500`).
- **Rich CLI.** Colored, grouped `--help` for every command, `-x` and
  `--long` spellings for flags, `--json` for data commands, and generated
  zsh/bash/fish completion.
- **Robust error handling.** Every dial, handshake, fetch and probe returns a
  descriptive error; the caching pipeline reports precise failure reasons and
  never overwrites a good cache with an empty result.
- **Offline-capable.** The consolidated proxy list is embedded in the binary
  (`-builtin`), so you can build a cache with no network source.

---

## Install

Download a release binary, or build from source (Go 1.24+):

```sh
git clone https://github.com/Szmelc-INC/SilkRouter
cd SilkRouter
make install          # no root needed
```

`make install` puts `silkrouter` plus the `px` and `s-proxy` symlinks into
`~/.local/bin`, and shell completions under `~/.local/share` (zsh, bash,
fish). Make sure `~/.local/bin` is on your `PATH`. Other locations:

```sh
make install PREFIX=/opt/silkrouter            # any prefix you can write to
make install PREFIX=/usr DESTDIR="$pkgdir"     # packaging (staged install)
make uninstall                                 # same PREFIX/DESTDIR rules
```

Only `make build` is needed for a local binary (`./silkrouter`); copy or
symlink it under any of the three names. The resulting binary has **no
runtime dependencies**. Cross-compile everything into `dist/` with
`make release`.

### Shell completion

`make install` installs completion for all three names. To load it by hand:

```sh
source <(px completion zsh)     # zsh (or save as _silkrouter on $fpath)
source <(px completion bash)    # bash
px completion fish | source     # fish
```

For zsh, add `~/.local/share/zsh/site-functions` to `fpath` before `compinit`
if you installed with the default prefix. `make completions` writes the three
scripts to `completions/` (generated, not committed).

---

## Quick start

```sh
# 1. Build a tested, sorted cache (defaults to the upstream proxy list)
px build

# 2. Route a command through a random cached proxy
px curl -s https://ifconfig.me          # px.sh style
px route -- curl -s https://ifconfig.me # explicit

# 3. Inspect the cache and the config home
px list
px paths
```

---

## Commands

```
px [global options] <command> [options] [args]
px [route options] <program> [args...]     # legacy px.sh style: same as route
px -- <program> [args...]                  # always route
px -B | -R | -L | -F FILE                  # px.sh modes: build, recache, list, build -F

Routing        route, run     run a command through a cached/random proxy
Cache          build          build/append the proxy cache (test & sort proxies)
               recache        re-test the proxies already in the cache (prune dead)
               list, ls       print a report of the current cache
Checks         check          test a single proxy or a file of proxies
               blcheck        check IP(s) against ~60 DNS blacklists (mxtoolbox)
               proxycheck     check proxy/anonymity detection (whatismyipaddress)
               probes         list the available caching probes
Configuration  config, cfg    show and edit the configuration (config.toml)
               paths, home    print the config home and every file in it
Help & info    completion     print a shell completion script (zsh, bash, fish)
               help           show help for the program or a command
               version        print the version
```

Run `px help <command>` or `px <command> --help` for every flag, its default
and the config key behind it. Every flag works with one or two dashes
(`-probe` = `--probe`), and most have a short and a long name (`-d` =
`--max-ms`).

Global options (before the command; `--no-color`, `-q` and `-v` also after
it): `--config FILE`, `--home DIR`, `--no-color` (or `NO_COLOR=1`),
`-q/--quiet`, `-v/--verbose`, `--version`, `-h/--help`. Note that `-v` means
verbose now; print the version with `--version` or `px version`.

The text cache lives at `~/.config/szmelc/proxy/cache.list` unless `$PX_LIST`,
the `cache.file` config key or `-f FILE` says otherwise. On first run, an
existing pre-v4 cache (`$PX_LIST` or `<tmp>/silkrouter-proxy.list`) is copied
there. Its format is identical to the old tool, so caches are interchangeable:

```
scheme://host:port [Nms]     # sorted fastest -> slowest
```

### `build` / `recache` — build the cache

```sh
px build [options]
px recache [options]        # re-test current cache, prune dead
```

| Flag                     | Description                                         | Default (config key) |
|--------------------------|-----------------------------------------------------|----------------------|
| `-f, --cache FILE`       | Cache file                                          | `cache.list` in the config home (`cache.file`) |
| `-u, --url URL`          | Source proxy-list URL                               | upstream list (`source.url`) |
| `-F, --from-file FILE`   | Build from a local proxy-list file                  | — |
| `--builtin`              | Use the proxy list embedded in the binary (offline) | off (`source.builtin`) |
| `--probe NAME`           | Caching probe (see `px probes`)                     | `cloudflare` (`probe.name`) |
| `--test-url URL`         | Custom probe URL (any 2xx = alive)                  | — (`probe.test_url`) |
| `-j, --workers N`        | Parallel workers                                    | `200` (`build.workers`) |
| `-n, --tries N`          | Samples averaged per proxy                          | `2` (`build.tries`) |
| `-w, --timeout DUR`      | Per-try timeout (`15`, `15s`, `500ms`)              | `15s` (`build.timeout`) |
| `-a, --show-dead`        | Also log DEAD proxies                               | off (`build.show_dead`) |
| `-O, --overwrite`        | Overwrite instead of append-merging                 | — |

`build` and `recache` **append-merge** by default (new timings win on
duplicates) and re-sort. `recache` always overwrites (it prunes dead entries).

### `route` — run a command through a proxy

```sh
px route [options] [--] <command...>
px [options] <command...>              # same thing, px.sh style
```

Exports `ALL_PROXY` / `HTTP_PROXY` / `HTTPS_PROXY` (and lowercase variants) for
the child process, then runs it.

| Flag                  | Description                                     | Default (config key) |
|-----------------------|-------------------------------------------------|----------------------|
| `-f, --cache FILE`    | Cache file                                      | `cache.list` in the config home (`cache.file`) |
| `-d, --max-ms MS`     | Max cached latency                              | `1000` (`route.max_ms`) |
| `-D, --min-ms MS`     | Min cached latency                              | `0` (`route.min_ms`) |
| `-H, --http`          | HTTP/HTTPS proxies only                         | `route.scheme = "http"` |
| `-s, --socks`         | SOCKS proxies only                              | `route.scheme = "socks"` |
| `-p, --proxy URL`     | Force a specific proxy (skips list & filters)   | — |
| `-x, --no-verify`     | Skip verification — fire through the first pick | default for cache (`route.verify`) |
| `-c, --verify`        | Verify candidates until one answers             | default for `-r` (`route.verify`) |
| `-t, --timeout DUR`   | Verify timeout                                  | `8s` (`route.verify_timeout`) |
| `-r, --fresh`         | Ignore cache, grab a fresh proxy from the source| — |
| `-v, --verbose`       | Print the exit IP before running                | — |
| `--probe NAME`        | Probe used for verification                     | `cloudflare` (`probe.name`) |
| `--build`             | Auto-build the cache if missing (no prompt)     | off (`route.auto_build`) |

```sh
px -s -d 500 git pull                          # socks-only, <500ms
px -p socks5://1.2.3.4:1080 wget https://example.com
px -r -v curl https://ifconfig.me              # fresh proxy, show exit IP
px -- build.sh                                 # a program named like a subcommand
```

### `check` — test proxies

```sh
px check [options] <proxy | file>
```

Measures liveness and latency with the selected probe. A single argument that
is an existing file is bulk-tested; otherwise it's treated as one proxy.
`--json` prints machine-readable results.

```sh
px check socks5://1.2.3.4:1080
px check -probe ipify proxies.txt      # also prints exit IPs
```

### `blcheck` — DNS blacklist check

```sh
px blcheck [options] <ip>
px blcheck -f <file>            # writes <file>-BL
```

| Flag                | Description                                   | Default (config key) |
|---------------------|-----------------------------------------------|----------------------|
| `-f, --file FILE`   | Check every address in `FILE`                 | — |
| `--token T`         | mxtoolbox temp token (`$MXTB_TOKEN` also read)| auto-fetch (`blcheck.token`) |
| `-d, --delay DUR`   | Delay between lookups in file mode            | `2s` (`blcheck.delay`) |
| `--timeout DUR`     | HTTP timeout per lookup                       | `20s` (`blcheck.timeout`) |
| `-v, --verbose`     | Single-IP: verbose listing                    | — |

### `proxycheck` — proxy/anonymity detection

```sh
px proxycheck [options] <ip|ip:port|proto://ip:port>
px proxycheck -f <file> [-o out]    # writes <file>-PX
```

Routes through the given proxy and reports `CLEAN` / `DETECTED` / `BLOCKED`
(Cloudflare challenge) / `FAIL`. Accepts the same mixed line formats as the
cache, including latency-annotated entries.

---

## Configuration

All state lives in one directory, the **config home**:
`$SILKROUTER_HOME`, or `${XDG_CONFIG_HOME:-~/.config}/szmelc/proxy`
(`--home DIR` sets it for one run). `px paths` prints the layout:

| Path            | Content                                         |
|-----------------|-------------------------------------------------|
| `config.toml`   | user configuration                              |
| `silkrouter.db` | proxy database (upcoming)                       |
| `lists/`        | downloaded reputation / known-proxy lists       |
| `logs/`         | plain-text logs                                 |
| `shell/`        | generated shell init snippets                   |
| `run/`          | runtime state                                   |
| `cache.list`    | the text cache                                  |

Precedence: built-in defaults < `config.toml` < environment < command-line
flags. Every key can be set from the environment as
`SILKROUTER_<TABLE>_<KEY>` (`route.max_ms` → `SILKROUTER_ROUTE_MAX_MS`);
the legacy `PX_LIST` (`cache.file`) and `MXTB_TOKEN` (`blcheck.token`) still
work. Durations accept seconds (`8`, `0.5`) or Go durations (`8s`, `1m30s`).

```sh
px config init                     # write a commented default config.toml
px config set route.max_ms 500     # validated; keeps config.toml.bak
px config get route.max_ms
px config show --sources           # effective values and where they come from
px config show --json
px config keys                     # every key: type, default, env variable
px config reset route.max_ms       # one key (or all, without arguments)
px config edit                     # $VISUAL / $EDITOR
px config path
```

---

## Caching probes

The **probe** is how SilkRouter decides a proxy is alive and how it measures
latency. List them with `silkrouter probes`:

| Probe        | Endpoint                                   | Notes |
|--------------|--------------------------------------------|-------|
| `cloudflare` | `cp.cloudflare.com/generate_204`           | default, fast |
| `google`     | `www.google.com/generate_204`              | |
| `gstatic`    | `connectivitycheck.gstatic.com/generate_204` | |
| `msft`       | `www.msftconnecttest.com/connecttest.txt`  | |
| `apple`      | `captive.apple.com/hotspot-detect.html`    | |
| `example`    | `example.com`                              | IANA-reserved, very stable |
| `ipify`      | `api.ipify.org`                            | also reports exit IP |
| `aws`        | `checkip.amazonaws.com`                     | also reports exit IP |
| `icanhazip`  | `icanhazip.com`                            | also reports exit IP |

Or bring your own: `-test-url https://my.endpoint/health` (any `2xx` counts).

---

## Project layout

```
main.go             boot: names, legacy px.sh modes, first-run hook, embedded list
cmd_*.go            one file per command group, self-registered from init()
internal/
  cli/              command registry, dispatcher, help renderer, completions
  config/           typed TOML config: defaults, file, env, get/set, writer
  paths/            config home resolution and layout
  proxy/            Proxy type, parsing, http/https/socks4/socks5 dialers
  probe/            caching probes (registry — add methods here)
  source/           candidate lists (URL / file / embedded)
  cache/            build (worker pool), merge, load, save, report
  router/           proxy selection + command execution
  scan/             blcheck + proxycheck HTML scrapers
  ui/               cross-platform colour handling
legacy/             original bash scripts (px.sh, blcheck.sh, proxycheck.sh)
proxy/              free proxy sources + consolidated LIST.txt (embedded)
```

**Adding a new probe:** append one `register(Probe{...})` in
`internal/probe/probe.go`. **Adding a new protocol:** extend the switch in
`internal/proxy/dial.go` and add its handshake. **Adding a command:** create a
`cmd_<name>.go` that calls `cli.Register(cli.Command{...})` from `init()`;
help and completion pick it up automatically. **Adding a config key:** add a
tagged field to the struct in `internal/config/config.go` and its default to
`Default()`. See [`docs/ARCHITECTURE-v4.md`](docs/ARCHITECTURE-v4.md).

---

## Development

```sh
make build        # host binary
make run ARGS="probes"
make test         # go test ./...
make test-race    # race detector (needs a C toolchain)
make check        # fmt-check + vet + test
make completions  # zsh/bash/fish scripts -> completions/
make install      # binary + px/s-proxy symlinks + completions -> ~/.local
make release      # cross-compile all platforms -> dist/
make update       # git pull + rebuild
```

---

## Notes

- Free proxy lists have high failure rates — a few hundred live proxies out of
  thousands of candidates is normal.
- Latency is a full round-trip through the proxy to the probe endpoint, not a
  ping; proxies are sorted fastest → slowest.
- Cached proxies are trusted by default to save time (`-x`); use `-c` to have
  `route` verify candidates until one answers.
- `proxycheck` checks the exit node you route *through*; datacenter-range exits
  often trip Cloudflare and return `BLOCKED` rather than a real verdict.
