# SilkRouter v2
Dependency-light bash script that **builds a fast local proxy cache**
and **routes any command through a random proxy** from it.

No frameworks, no daemons — just `curl`, `awk`, `xargs`, `shuf` and `sort`.

---

## Install
- Put `px.sh` into `$PATH` (for example in `/bin/px`), then `chmod +x $(whereis px)`
- Or add `alias px='bash /path/to/px.sh'` to your runcommand (`.bashrc` / `.zshrc`)

---

## Quick start

```sh
px curl https://ifconfig.me     # first run: offers to build a cache, then routes
px curl https://ifconfig.me     # later runs: instant pick from the cache
px -B                           # (re)build the cache whenever you want
```

On the **first run** (no cache at `/tmp/proxy.list`) it asks:

```
px: no cache at /tmp/proxy.list. Build a tested, sorted cache for best routes? [Y/n]
```

- **Y** (default) — tests the proxy list, measures each one's average delay, and
  writes the working ones sorted fastest → slowest. Subsequent runs are instant.
- **n** — skips caching and grabs a fresh (untested) proxy straight from the
  source for this run, verifying candidates until one answers.

---

## Usage

### Route a command

```sh
px curl https://example.com        # random proxy <=1000ms, fire immediately
px -H wget https://example.com     # http proxies only
px -s youtube-dl ...               # socks proxies only
px -D 200 -d 500 curl ...          # only proxies in the 200-500ms window
px -v curl ...                     # also print the exit IP
px -c curl ...                     # verify candidates until one answers
px -p socks5://1.2.3.4:1080 ...    # force a specific proxy
px -r curl ...                     # ignore cache, use a fresh proxy from source
```

Every invocation prints the route + process to stderr (command stdout stays
clean):

```
[px] proxy: socks5 203.25.208.163:1011  |  cmd: curl  pid: 12345
```

By default the cached pick is trusted and used as-is (`-x`). Fresh (`-r`) picks
are untested, so those verify-until-one-answers by default; `-c`/`-x` override
either way.

### Build / refresh the cache

```sh
px -B                     # download, test, write sorted /tmp/proxy.list
px -B -a                  # also log DEAD proxies while testing
px -B -w 30 -j 200        # 30s per-ping timeout, 200 parallel workers
px -B -u <URL>            # use a different proxy-list source
```

The build runs until every proxy answers or hits the per-ping timeout (`-w`,
default 60s) — no overall deadline. Results are collected in a temp file and
sorted into the output only at the end, so the list is always ordered by delay.

Set `PX_LIST` (or `-f FILE`) to use a different cache path.

---

## Options

| Flag | Meaning | Default |
|------|---------|---------|
| `-f FILE` | cache file path | `$PX_LIST` or `/tmp/proxy.list` |
| `-d MAX` / `-D MIN` | delay window (ms) | `1000` / `0` |
| `-H` / `-s` | http-only / socks-only | any |
| `-p PROXY` | force a specific proxy | — |
| `-x` / `-c` | skip / force verification | cache=skip, fresh=verify |
| `-t SEC` | verify timeout | `8` |
| `-r` | ignore cache, fresh from source | — |
| `-v` | print exit IP | off |
| `-B` | build cache and exit | — |
| `-u URL` | source URL (build) | proxyscrape free list |
| `-j N` / `-n N` / `-w SEC` | jobs / samples / per-ping timeout (build) | `50` / `2` / `60` |
| `-a` | log DEAD proxies (build) | off |

---

## How delay is measured

"Delay" is the round-trip of a real request *through* the proxy (`curl --proxy`,
averaged over `-n` samples) — not ICMP, which is meaningless through a proxy. The
default target is a tiny `generate_204` endpoint over plain HTTP, so the number
reflects the proxy hop rather than a TLS handshake.

Cache format (one proxy per line):

```
socks5://203.25.208.163:1011 [142ms]
http://43.133.169.167:3128 [310ms]
```

---

## Notes

- Free proxy lists are mostly dead or slow; a cache of a few hundred working
  entries out of a couple thousand candidates is normal.
- Delay confirms reachability and latency, not throughput or whether the proxy
  forwards traffic faithfully. Verify exits yourself where that matters.
- Skipping verification (the cache default) means a proxy that died since the
  last build will make the command fail rather than silently re-rolling; pass
  `-c` if you'd rather it hunt for a live one.
- Use responsibly and only against systems you're authorized to test.

## Requirements

`bash` 4+, `curl`, `awk`, `xargs`, `shuf`, `sort`.
