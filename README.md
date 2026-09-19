# SilkRouter

A single, dependency-free, cross-platform **Go binary** that builds a fast
local proxy cache and routes any command through a random proxy — plus built-in
blacklist and proxy/anonymity checkers.

SilkRouter v3 is a full rewrite of the original bash toolkit (`px.sh`,
`blcheck.sh`, `proxycheck.sh`) into one static binary. No `curl`, no `awk`, no
`shuf` — nothing to install. It cross-compiles to Linux, Windows, macOS (amd64
and arm64) with `CGO_ENABLED=0`.

> The original shell scripts are preserved under [`legacy/`](legacy/).

---

## Highlights

- **One binary, all tools.** `build`, `route`, `check`, `blcheck`,
  `proxycheck` — the whole toolkit in a single subcommand-driven executable.
- **All protocols, natively.** `http`, `https`, `socks4`, `socks4a` and
  `socks5` are dialed by hand-rolled, standard-library clients — including
  SOCKS4/4a which most Go proxy libraries don't support.
- **Many ways to cache.** Validation is no longer hard-wired to Cloudflare.
  Pick from nine built-in **probes** (Cloudflare, Google, gstatic, Apple,
  Microsoft, example.com, ipify, AWS, icanhazip) or supply your own
  `-test-url`. Adding a new probe is a one-line registry entry.
- **Robust error handling.** Every dial, handshake, fetch and probe returns a
  descriptive error; the caching pipeline reports precise failure reasons and
  never overwrites a good cache with an empty result.
- **Offline-capable.** The consolidated proxy list is embedded in the binary
  (`-builtin`), so you can build a cache with no network source.
- **Easy to extend.** Clean `internal/` packages: `proxy` (dialers),
  `probe` (validation methods), `source` (candidate lists), `cache`
  (build/merge/report), `router` (command execution), `scan` (checkers).

---

## Install

Download a release binary, or build from source:

```sh
git clone https://github.com/Szmelc-INC/SilkRouter
cd SilkRouter
make build            # -> ./silkrouter
sudo make install     # -> /usr/local/bin/silkrouter
```

Requires Go 1.24+ to build. The resulting binary has **no runtime
dependencies**.

Cross-compile everything into `dist/`:

```sh
make release
```

---

## Quick start

```sh
# 1. Build a tested, sorted cache (defaults to the upstream proxy list)
silkrouter build

# 2. Route a command through a random cached proxy
silkrouter route -- curl -s https://ifconfig.me

# 3. Inspect the cache
silkrouter list
```

---

## Commands

```
silkrouter <command> [options]

  route      run a command through a cached/random proxy
  build      build/append the proxy cache (test & sort proxies)
  recache    re-test the proxies already in the cache (prune dead)
  list       print a report of the current cache
  check      test a single proxy or a file of proxies
  blcheck    check IP(s) against ~60 DNS blacklists (mxtoolbox)
  proxycheck check proxy/anonymity detection (whatismyipaddress)
  probes     list the available caching probes
  version    print the version
  help       show this help
```

Run `silkrouter <command> -h` for command-specific flags.

The cache lives at `$PX_LIST`, or a temp file
(`<tmp>/silkrouter-proxy.list`) otherwise. Its format is identical to the old
tool, so caches are interchangeable:

```
scheme://host:port [Nms]     # sorted fastest -> slowest
```

### `build` / `recache` — build the cache

```sh
silkrouter build [options]
silkrouter recache [options]        # re-test current cache, prune dead
```

| Flag         | Description                                        | Default |
|--------------|----------------------------------------------------|---------|
| `-f FILE`    | Cache file                                         | `$PX_LIST` or temp |
| `-u URL`     | Source proxy-list URL                              | upstream list |
| `-F FILE`    | Build from a local proxy-list file                 | — |
| `-builtin`   | Use the proxy list embedded in the binary (offline)| — |
| `-probe NAME`| Caching probe (see `silkrouter probes`)            | `cloudflare` |
| `-test-url U`| Custom probe URL (any 2xx = alive)                 | — |
| `-j N`       | Parallel workers                                   | `200` |
| `-n N`       | Samples averaged per proxy                         | `2` |
| `-w SEC`     | Per-try timeout                                    | `15` |
| `-a`         | Also log DEAD proxies                              | — |
| `-O`         | Overwrite instead of append-merging                | — |

`build` and `recache` **append-merge** by default (new timings win on
duplicates) and re-sort. `recache` always overwrites (it prunes dead entries).

### `route` — run a command through a proxy

```sh
silkrouter route [options] [--] <command...>
```

Exports `ALL_PROXY` / `HTTP_PROXY` / `HTTPS_PROXY` (and lowercase variants) for
the child process, then runs it.

| Flag         | Description                                          | Default |
|--------------|------------------------------------------------------|---------|
| `-f FILE`    | Cache file                                            | `$PX_LIST` or temp |
| `-d MAX`     | Max delay ms                                          | `1000` |
| `-D MIN`     | Min delay ms                                          | `0` |
| `-H`         | HTTP/HTTPS proxies only                               | — |
| `-s`         | SOCKS proxies only                                    | — |
| `-p PROXY`   | Force a specific proxy (skips list & filters)         | — |
| `-x`         | Skip verification — fire through the first pick       | default for cache |
| `-c`         | Verify candidates until one answers                   | default for `-r` |
| `-t SEC`     | Verify timeout                                        | `8` |
| `-r`         | Ignore cache, grab a fresh proxy from the source      | — |
| `-v`         | Print the exit IP before running                      | — |
| `-probe N`   | Probe used for verification                           | `cloudflare` |
| `-build`     | Auto-build the cache if missing (no prompt)           | — |

```sh
silkrouter route -s -d 500 -- git pull          # socks-only, <500ms
silkrouter route -p socks5://1.2.3.4:1080 -- wget https://example.com
silkrouter route -r -v -- curl https://ifconfig.me   # fresh proxy, show exit IP
```

### `check` — test proxies

```sh
silkrouter check [options] <proxy | file>
```

Measures liveness and latency with the selected probe. A single argument that
is an existing file is bulk-tested; otherwise it's treated as one proxy.

```sh
silkrouter check socks5://1.2.3.4:1080
silkrouter check -probe ipify proxies.txt      # also prints exit IPs
```

### `blcheck` — DNS blacklist check

```sh
silkrouter blcheck [options] <ip>
silkrouter blcheck -f <file>            # writes <file>-BL
```

| Flag        | Description                                  | Default |
|-------------|----------------------------------------------|---------|
| `-f FILE`   | Check every address in `FILE`                | — |
| `-token T`  | mxtoolbox temp token (`$MXTB_TOKEN` also read)| auto-fetch |
| `-d SEC`    | Delay between lookups in file mode           | `2` |
| `-v`        | Single-IP: verbose listing                   | — |

### `proxycheck` — proxy/anonymity detection

```sh
silkrouter proxycheck [options] <ip|ip:port|proto://ip:port>
silkrouter proxycheck -f <file> [-o out]    # writes <file>-PX
```

Routes through the given proxy and reports `CLEAN` / `DETECTED` / `BLOCKED`
(Cloudflare challenge) / `FAIL`. Accepts the same mixed line formats as the
cache, including latency-annotated entries.

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
main.go             CLI dispatch + embedded proxy list
cmd_*.go            subcommand flag parsing & output
internal/
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
`internal/proxy/dial.go` and add its handshake.

---

## Development

```sh
make build        # host binary
make run ARGS="probes"
make test         # go test ./...
make test-race    # race detector (needs a C toolchain)
make check        # fmt-check + vet + test
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
