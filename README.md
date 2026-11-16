## CONSOLIDATED DELIVERABLE

# SilkRouter

![Go](https://img.shields.io/badge/Go-1.26-105c5c?logo=go&logoColor=white)
![License: MIT](https://img.shields.io/badge/license-MIT-105c5c)
![Platforms](https://img.shields.io/badge/platforms-linux%20%7C%20macos%20%7C%20windows%20%7C%20freebsd-c0c0c0)

SilkRouter is a single, zero-dependency Go binary for managing and routing network traffic through proxies. It stores proxies in an embedded SQLite database, scans and ranks them, routes single commands or entire shell sessions through the best proxy, and logs every execution.

It completely replaces legacy shell toolkits (`px.sh`, `blcheck.sh`, `proxycheck.sh`). You do not need `curl`, `awk`, or `shuf`. It cross-compiles to Linux, macOS, Windows, and FreeBSD (`CGO_ENABLED=0`).

The binary responds to three command names: **`px`**, **`s-proxy`**, and **`silkrouter`**. Backward compatibility with original `px.sh` syntax (`px curl ifconfig.me`, `px -d 500 -s git pull`, `px -B`) is fully preserved.

> The legacy shell scripts remain preserved under [`legacy/`](legacy/).

---

## Highlights

- **One binary, three commands:** `px`, `s-proxy`, and `silkrouter` are identical. Every feature is accessible as a subcommand with grouped, colorized help.
- **Single data directory:** Configs, SQLite database, downloaded IP lists, logs, shell scripts, and active sessions live in `~/.config/szmelc/proxy/`.
- **Embedded SQLite database:** Uses a pure-Go SQLite driver to track ~12k proxies along with test results, reputation verdicts, usage counts, and tags.
- **Deep proxy scanning:** `px scan full` tests latency, jitter, real exit IPs, HTTPS support, anonymity levels (transparent, anonymous, elite), protocol support, and checks IPs against 15+ DNS blacklists and abuse feeds. Pauses and resumes cleanly with Ctrl+C.
- **Smart scoring & ranking:** Calculates speed, cleanliness, and stability scores ($0$ to $100$) for every active proxy. Pick proxies by fastest, cleanest, or balanced ranks.
- **Complete usage log:** Records every proxied command (timestamp, proxy used, command line, working directory, exit code, duration, latency). Run `px stats` to view reports by proxy, command, or date.
- **Automatic & custom tags:** Auto-tags proxies (`stable`, `fast`, `clean`, `elite`, `tor`, `datacenter`, `flaky`, `mitm`). Add custom tags like `blocked` to skip bad proxies.
- **Smart routing:** Route single commands with `px <cmd>`. Works with `--best`, `--tag fast,elite`, `--for git`, or `--chain` (proxychains-ng for full process trees).
- **Shell integration:** Route traffic for your current shell (`px on`, `px off`, `px rotate`). Displays active proxy status in your shell prompt (Powerlevel10k, zsh, bash, fish).
- **Interactive TUI:** Run `px tui` for a terminal dashboard to inspect, scan, and manage proxies with a Vim-style `:` command palette.
- **Native multi-protocol support:** Built-in HTTP, HTTPS, SOCKS4, SOCKS4a, and SOCKS5 client dialers using standard Go libraries.
- **Ready to package:** Includes self-documenting Makefile targets, an Arch Linux `PKGBUILD`, and a Nix Flake.

---

## Install

### From source (Go 1.26+)

```sh
git clone https://github.com/Szmelc-INC/SilkRouter
cd SilkRouter
make                  # list all build targets
make install          # installs to ~/.local/bin (no root needed)
silkrouter shell install
```

`make install` places `silkrouter` and its symlinks (`px`, `s-proxy`) into `~/.local/bin`. Shell completions go to `~/.local/share` (zsh, bash, fish), and integration scripts go to `~/.local/share/silkrouter`.

To install to custom paths:

```sh
make install PREFIX=/opt/silkrouter            # install to custom prefix
make install PREFIX=/usr DESTDIR="$pkgdir"     # staged packaging build
make uninstall                                 # remove installed files
```

The binary has **zero runtime dependencies** (SQLite is built-in). Run `make release` to cross-compile for all supported platforms into `dist/` with `SHA256SUMS`.

### Arch Linux

```sh
cd packaging/arch && makepkg -si      # or: make pkg-arch
```

The [`packaging/arch/PKGBUILD`](packaging/arch/PKGBUILD) script builds `silkrouter-git` with completions, shell scripts, and docs. Optional dependencies: `proxychains-ng` (for `--chain`) and `zsh-theme-powerlevel10k`.

### Nix

```sh
nix run github:Szmelc-INC/SilkRouter -- --version
nix profile install github:Szmelc-INC/SilkRouter
nix develop            # dev environment: go, gopls, golangci-lint, make, sqlite, proxychains
```

The flake provides packages, apps, development shells, overlays, and a Home Manager module:

```nix
imports = [ silkrouter.homeManagerModules.default ];
programs.silkrouter = {
  enable = true;                       # enables zsh / bash / fish integration
  settings.route.max_ms = 500;         # writes to ~/.config/szmelc/proxy/config.toml
};
```

Update `vendorHash` in [`packaging/nix/package.nix`](packaging/nix/package.nix) by running `make nix-hash` after modifying `go.mod`.

### Shell completion

Completions are installed automatically by packages and `make install`. To load manually:
- **Zsh:** `source <(px completion zsh)`
- **Bash:** `source <(px completion bash)`
- **Fish:** `px completion fish | source`

---

## Quick start

```sh
px scan quick                        # fast check: test all proxies for availability and latency
px scan full --status alive          # deep check: scan working proxies and update ranks
px proxies --sort rank -n 20         # list top 20 proxies by rank
px show 42                           # view detailed record for proxy #42

px curl -s https://ifconfig.me       # run one command through recommended proxy
px --best --tag elite yt-dlp URL     # route through top-ranked elite proxy

px on                                # enable proxying in current shell (requires shell install)
px history                           # view command execution history
px stats --by proxy,cmd -s 7d        # view usage statistics for the past week
px off                               # disable shell proxying

px tui                               # launch interactive terminal interface
```

---

## Commands Summary

```
px [global options] <command> [options] [args]
px [route options] <program> [args...]     # legacy syntax (alias for route)
px -- <program> [args...]                  # force routing mode
px -B | -R | -L | -F FILE                  # legacy modes: build, recache, list, build file
```

| Category | Command | Description |
|---|---|---|
| **Routing** | `route`, `run` | Run a command through a selected proxy |
| **Cache** | `build` | Test candidates and build/append the proxy cache |
| | `recache` | Re-test cached proxies and prune dead entries |
| | `list`, `ls` | Display working proxies from database or text file |
| **Database** | `db` | Manage the SQLite proxy database |
| | `proxies`, `db-ls` | Query, filter, sort, and page stored proxies |
| | `show`, `inspect` | Print all stored facts for a specific proxy |
| **Scanning** | `scan` | Run scans: `quick`, `full`, `reputation`, `geo`, `protocols`, `runs` |
| | `rank` | Recalculate speed, clean, and stability ranks |
| | `lists` | Manage IP blacklists and reputation feeds |
| **Checks** | `check` | Test a single proxy address or a list file |
| | `blcheck` | Check IP against ~60 DNS blacklists via mxtoolbox |
| | `proxycheck` | Check IP anonymity via whatismyipaddress |
| | `probes` | Print available latency probe endpoints |
| **History & Tags** | `history`, `log` | View proxied command execution logs |
| | `stats` | Display usage metrics grouped by command, proxy, or error |
| | `tags`, `tag` | Manage proxy tags (view, add, remove, refresh) |
| | `favorites`, `favs` | List favorite proxies per command (`fav:<cmd>`) |
| | `recommend`, `rec` | Show recommended proxies overall or for a command |
| **Shell** | `on` / `off` | Enable or disable proxying in current shell session |
| | `rotate` | Switch active shell session to a different proxy |
| | `relay` | Start local SOCKS/HTTP proxy server with upstream rotation |
| | `session`, `status` | View, manage, or prune shell proxy sessions |
| | `shell` | Launch a proxied subshell |
| | `shell-init` | Print shell integration script (zsh, bash, fish) |
| | `prompt` | Output prompt segment for prompt generators |
| **Interface** | `tui`, `ui` | Launch interactive terminal interface |
| **Configuration**| `config`, `cfg` | Read, set, or edit configuration settings |
| | `paths`, `home` | Print configuration home directory layout |
| **Help & Info** | `completion` | Output shell completion scripts |
| | `help`, `version` | Display help or version info |

View detailed command flags by running `px help <command>` or `px <command> --help`. 

Global options: `--config FILE`, `--home DIR`, `--no-color`, `-q/--quiet`, `-v/--verbose`, `--version`, `-h/--help`.

---

## Detailed Feature Usage

### Routing commands: `route`

```sh
px route [options] [--] <command...>
px [options] <command...>              # shorthand routing syntax
```

`px route` selects a matching active proxy from the database, sets standard proxy environment variables (`ALL_PROXY`, `HTTP_PROXY`, `HTTPS_PROXY` in uppercase and lowercase), and executes the given command.

Selection modes (`route.prefer`):
- **`smart`** (default): prefers proxies tagged `recommended:<cmd>`, then `fav:<cmd>`, then `recommended`, then remaining matches (randomized within each tier).
- **`rank`**: selects the highest-ranked proxy.
- **`random`**: selects randomly from matching proxies.

Proxies tagged `blocked` (`route.exclude_tags`) are skipped automatically.

| Flag | Description | Default (Config key) |
|---|---|---|
| `-d, --max-ms MS` | Maximum proxy latency | `1000` (`route.max_ms`) |
| `-D, --min-ms MS` | Minimum proxy latency | `0` (`route.min_ms`) |
| `-H, --http` / `-s, --socks` | Protocol filter | `route.scheme` |
| `-T, --tag TAGS` | Require specific tags | — |
| `--exclude-tag TAGS` | Exclude specific tags | `blocked` (`route.exclude_tags`) |
| `-b, --best` | Pick highest-ranked proxy (`--prefer rank`) | — |
| `--for CMD` | Use recommendations for specified command | current binary |
| `--prefer ORDER` | Selection order: `smart`, `rank`, `random` | `smart` (`route.prefer`) |
| `--chain` | Execute via proxychains-ng (proxies entire process tree) | `route.method` |
| `-p, --proxy URL` | Force specific proxy URL | — |
| `-x` / `-c` | Skip / force connectivity verification before run | `route.verify` |
| `-t, --timeout DUR` | Verification timeout | `8s` (`route.verify_timeout`) |
| `-r, --fresh` | Fetch fresh proxy candidate | — |
| `-v, --verbose` | Print proxy info and exit IP before executing | — |
| `--no-log` | Skip recording run in history log | `history.record` |
| `-f, --cache FILE` | Use legacy text file cache instead of database | `cache.file` |

---

### Scanning & Ranking: `scan`, `rank`, `lists`

```sh
px scan full                         # run full checks on all stored proxies (resumable)
px scan full --status alive -j 64    # scan active proxies with 64 parallel workers
px scan quick                        # fast single-probe latency check
px scan runs                         # view progress of active or paused scan runs
px rank [-m balanced|speed|clean|stable] [--top 30]
px lists update                      # refresh IP blacklists and reputation feeds
```

#### What `scan full` checks:
1. **Latency & Jitter:** Measures TCP connect time and latency variance across configured probes.
2. **TLS Interception (MITM):** Conducts HTTPS handshakes. Proxies presenting custom TLS certificates are tagged `mitm` (excluded from route, shell, and relay by default).
3. **Anonymity Level:** Checks headers against your real IP to classify proxies as `transparent`, `anonymous`, or `elite`.
4. **Protocol Support:** Detects whether the port speaks HTTP, SOCKS4, or SOCKS5.
5. **Reputation & Geolocation:** Looks up host and exit IPs against Tor exits, datacenter networks, DNS blacklists, and geolocation providers.
6. **Scoring Formulas:**
   - **Speed Score:** $\text{Speed} = 100 \cdot \frac{\text{ref}}{\text{ref} + \text{median} + \frac{\text{jitter}}{2}}$ (where $\text{ref} = 250\text{ms}$).
   - **Clean Score:** Starts at $100$ and subtracts penalties for DNSBL listings, abuse lists, and low anonymity grades.
   - **Stability Score:** Smooths recent check success rates blended with historical command usage.

The balanced rank combines these scores using weights defined in `scan.weight_speed`, `scan.weight_clean`, and `scan.weight_stable`.

---

### History & Tagging: `history`, `stats`, `tags`, `favorites`, `recommend`

```sh
px history                           # list recent 50 executions
px history -c curl -s 24h            # list curl executions in past 24 hours
px history export --format csv -o usage.csv
px stats --by proxy,cmd              # print success rates and average latencies
px tags                              # list tags and proxy counts
px tags add 42 blocked               # tag proxy #42 as blocked
px recommend curl                    # print top recommended proxy for curl
```

#### Built-in Tag Definitions:

| Tag | Condition (`[tags]` config) |
|---|---|
| `stable` | Last `stable_n` (10) checks/uses all succeeded |
| `fast` | Last `fast_n` (10) successful runs were under `fast_ms` (100ms) |
| `fav:<cmd>` | $\ge 10$ uses for `<cmd>` with $\ge 80\%$ success rate |
| `recommended` | Top 5 overall ranked proxies |
| `recommended:<cmd>` | Top ranked proxies specific to `<cmd>` |
| `clean` | Fully scanned, zero blacklist hits |
| `elite` | Hides client IP completely (no proxy headers) |
| `https` | TLS connection succeeded |
| `flaky` | Frequent connection failures |
| `tor` / `datacenter` / `known` | IP matches Tor exit, hosting provider, or public proxy list |
| `mitm` | Intercepts TLS certificates (skipped by default) |

---

### Shell Integration: `on`, `off`, `rotate`, `shell`, `relay`

Because sub-processes cannot modify their parent shell's environment variables, SilkRouter uses shell functions to evaluate generated environment code.

#### Installation:
```sh
silkrouter shell install             # adds integration block to ~/.zshrc or ~/.bashrc
```

#### Usage:
```sh
px on [-s] [-d 300] [--best]          # enable proxying for current shell and sub-processes
px rotate                            # switch active session to another proxy
px status                            # show active session details
px off                               # restore original environment variables
px shell                             # launch a proxied subshell (exit to leave)
```

While active, typed commands are logged via `preexec` hooks (`shell.log_commands`), and your prompt displays status information:
- **Powerlevel10k:** Automatically adds a `silkrouter` prompt segment.
- **Plain Zsh / Bash / Fish:** Prefixes active proxy details to your prompt.
- **Starship / Tmux:** Use `px prompt` in custom shell prompt scripts.

---

### Local Relay Server: `relay`

```sh
px relay                             # run local proxy server on 127.0.0.1:18080
px relay --daemon --best             # run in background using best ranked proxies
px relay --rotate per-conn           # rotate upstream proxy for every connection
px relay --rotate interval --every 5m
eval "$(px relay env)"               # export local relay environment variables
px on --relay                        # route shell traffic through local relay
```

The local relay listens on one loopback port and accepts SOCKS5, SOCKS4/4a, and HTTP (CONNECT and plain) requests. It forwards traffic to upstream proxies selected from your database. If an upstream fails, the relay automatically fails over to another proxy.

---

### Interactive TUI: `tui`

Launch the terminal user interface with `px tui`:

- **Dashboard:** System overview and quick status.
- **Proxies:** Search, filter, inspect TLS details, and trigger scans on selected proxies.
- **History & Stats:** View execution logs and per-proxy metrics.
- **Scans & Lists:** Monitor scan progress and manage blacklist feeds.
- **Config Editor:** Modify configuration settings directly.
- **Command Palette:** Press `:` to execute TUI commands (e.g., `:scan full alive`, `:sort rank`). Press `?` for help.

---

## Configuration Reference

All settings are stored in **`~/.config/szmelc/proxy/`** (or `$SILKROUTER_HOME` / `--home DIR`).

```
~/.config/szmelc/proxy/
├── config.toml       # User configuration file
├── silkrouter.db     # SQLite database (WAL mode)
├── lists/            # Downloaded IP blacklists and metadata
├── logs/             # Text logs (usage.log)
├── shell/            # Generated shell scripts
├── run/              # Session data and temporary configs
└── cache.list        # Legacy text list cache
```

Configuration hierarchy: Default values < `config.toml` < Environment variables (`SILKROUTER_<TABLE>_<KEY>`) < CLI flags.

```sh
px config init                       # generate default config.toml
px config set scan.workers 48        # set config parameter
px config show --sources              # view effective configuration and sources
```

---

## Built-in Latency Probes

SilkRouter tests proxy latency against reliable endpoints:

| Probe | Target Endpoint | Notes |
|---|---|---|
| `cloudflare` | `cp.cloudflare.com/generate_204` | Default endpoint, fast response |
| `google` | `www.google.com/generate_204` | Google connectivity check |
| `gstatic` | `connectivitycheck.gstatic.com/generate_204` | Android connectivity check |
| `msft` | `www.msftconnecttest.com/connecttest.txt` | Windows network check |
| `apple` | `captive.apple.com/hotspot-detect.html` | Apple network check |
| `example` | `example.com` | IANA reserved domain |
| `ipify` | `api.ipify.org` | Latency check + exit IP discovery |
| `aws` | `checkip.amazonaws.com` | Latency check + exit IP discovery |
| `icanhazip` | `icanhazip.com` | Latency check + exit IP discovery |

Custom test endpoint: `px check -test-url https://example.com/health` (any HTTP 2xx response marks success).

---

## Development & Testing

```sh
make                                 # display available make targets
make build                           # compile local binary
make check                           # run fmt, vet, and tests (run before committing)
make cover                           # generate HTML coverage report
make release                         # build release binaries into dist/
```

---

## CONSOLIDATION & DECISION LOG

* **Primary Intent Inferred:** Completely rewrite the SilkRouter README text to be direct, approachable, and easy to read using short sentences and straightforward technical language while keeping all technical details, commands, tables, and functionality fully intact.
* **Architectural & Rebase Decisions:** 
  - Converted dense narrative paragraphs into structured tables, concise bullet points, and short sentences.
  - Formatted all scoring formulas using strict LaTeX notation ($100 \cdot \frac{\text{ref}}{\text{ref} + \dots}$).
  - Unified section headers for improved visual readability.
  - Standardized configuration paths and command summaries into clean Markdown tables.
* **Pruned Noise & Contradictions:** Removed repetitive intro commentary, consolidated redundant flag descriptions, and tightened technical command definitions.
* **Execution & Git Status:** All updates applied cleanly into `README.md`.
