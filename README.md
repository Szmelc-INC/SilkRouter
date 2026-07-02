# SilkRouter v2

A lightweight linux toolkit to help routing commands via proxy. \
It builds a local proxy cache and routes any command through a fast random proxy — plus companion DNS-blacklist and proxy-detection checkers.

---

## Contents
### Scripts
- [`px.sh`](px.sh) — Proxy router & cache updater
- [`blcheck.sh`](blcheck.sh) — IP blacklist checker
- [`proxycheck.sh`](proxycheck.sh) — Proxy/anonymity detection checker
### Extras
- [`Free sources`](proxy/README.md) - Collection of free proxy sources
- [`Big RAW list`](proxy/LIST.txt) - Consolidated RAW list of proxies. (Around 12k proxies in `protocol://ip:port` format.)
  
---

## Installation

1. Move the `.sh` scripts into your `$PATH` (e.g. `/bin`) and make them executable:
   ```sh
   chmod +x /bin/px.sh
   ...
   ```
   **or** alias them in your `.bashrc` / `.zshrc`:
   ```sh
   alias px='bash /path/to/px.sh'
   ...
   ```

**Dependencies:** `bash 4+` (or any other shell), `curl`, `awk`, `xargs`, `shuf`, `sort`, `grep` (with `-P`/PCRE support, for `proxycheck.sh`)

---

## `px.sh` - Proxy Router & Cache Builder

Build a fast local proxy cache and route any command through it.

```sh
px.sh [run-opts] <command...>   # route a command through a cached proxy
px.sh -B [build-opts]           # (re)build the cache and exit
```

### Run flags

| Flag | Description | Default |
|------|-------------|---------|
| `-f FILE` | Proxy list file | `$PX_LIST` or `/tmp/proxy.list` |
| `-d MAX` | Max delay (ms) | `1000` |
| `-D MIN` | Min delay (ms) | `0` |
| `-H` | HTTP proxies only | — |
| `-s` | SOCKS proxies only | — |
| `-p PROXY` | Force a specific proxy (skips list & filters) | — |
| `-x` | Skip verification — fire through first pick | default for cache |
| `-c` | Verify candidates until one answers | default for `-r` |
| `-t SEC` | Verify timeout | `8` |
| `-r` | Ignore cache, grab a fresh proxy from the source | — |
| `-v` | Print the exit IP (one request through the chosen proxy) | — |

### Build flags (with `-B`, or on first-run prompt)

| Flag | Description | Default |
|------|-------------|---------|
| `-u URL` | Source proxy-list URL | — |
| `-j N` | Parallel jobs | `50` |
| `-n N` | Samples averaged per proxy | `2` |
| `-w SEC` | Per-ping timeout | `60` |
| `-a` | Also log DEAD proxies | — |

The cache is written as `protocol://ip:port [Nms]`, sorted fastest → slowest.

---

## `blcheck.sh` - Blacklist Checker

Check IP(s) against ~60 DNS blacklists via mxtoolbox's web API.

```sh
blcheck.sh [options] <ip>
blcheck.sh [options] -f <file>
```

| Flag | Description | Default |
|------|-------------|---------|
| `-f, --file FILE` | Check every address in `FILE` (one per line). Writes `FILE-BL` with original lines + verdict appended. | — |
| `-t, --token TOK` | mxtoolbox tempauthorization token. Precedence: `-t` > `$MXTB_TOKEN` > auto-fetch from `/api/v1/user` | — |
| `-d, --delay SEC` | Delay between lookups in file mode | `$DELAY` |
| `-v, --verbose` | Single-IP mode: list every blacklist (green OK / red LISTED) | — |
| `-n, --no-color` | Disable colored output | — |
| `-h, --help` | Show help | — |

---

## `proxycheck.sh` - Proxy Detection Checker

Route a request through a given proxy and check whether it's flagged as a known proxy/anonymizer (rDNS, WIMIA, Tor, Geo-location, and HTTP-header heuristics) via whatismyipaddress.com's proxy-check.

```sh
proxycheck.sh -i <ip|ip:port|proto://ip:port>
proxycheck.sh -f <file> [-o <file>] [-t <sec>] [-d <sec>]
```

| Flag | Description | Default |
|------|-------------|---------|
| `-i, --ip TARGET` | Single check. Accepts `ip`, `ip:port`, or `proto://ip:port` | — |
| `-f, --file FILE` | Bulk check every proxy in `FILE` (one per line). Writes `FILE-PX` with original lines + verdict appended. | — |
| `-o, --output FILE` | Override output path for `-f` mode | `<file>-PX` |
| `-t, --timeout SEC` | Curl connect/max timeout | `8` |
| `-d, --delay SEC` | Delay between requests in bulk mode | `0` |
| `-h, --help` | Show help | — |

List entries accept mixed formats — raw `ip`, `ip:port`, `proto://ip:port`, and `proto://ip:port [Nms]` (latency-annotated lines from scraped lists, e.g. `px.sh -B` cache output, are stripped automatically). A bare `ip` with no port is assumed to be `http` on port `80`.

Per-entry verdicts: `CLEAN` (no proxy signals), `DETECTED` (flagged as proxy — strongest signal is a `WIMIA` hit), `BLOCKED` (target served a Cloudflare challenge instead of a result), `FAIL` (proxy unreachable/timed out).

---

## Notes

- First run (or if `/tmp/proxy.list` is missing) prompts to build a fresh proxy cache — recommended to do and redo somewhat often.
- Delay is measured via full curl round-trips through the proxy, not just pings; proxies are sorted fastest → slowest.
- Free proxy lists have high failure rates — a cache of 200 working proxies out of 2,000 candidates is normal.
- Cached proxies are trusted by default to save time; if a cached proxy recently died, your command will fail. Use `-c` to have the script hunt for a live one automatically.
- `proxycheck.sh` checks the exit node you route through, not an arbitrary target IP — it's only meaningful when pointed at something that's actually listening as a proxy (e.g. entries from `px.sh`'s cache). Datacenter-range exits are more likely to trip Cloudflare and come back `BLOCKED` rather than a real verdict.
