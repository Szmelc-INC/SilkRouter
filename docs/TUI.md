# SilkRouter TUI

`px tui` (alias `px ui`) is a full-screen interface over the database:
browse, filter and inspect every proxy, tag and block them, check them on
demand, read the usage log and its statistics, recompute ranks and edit the
configuration, and start, follow and pause scans. It needs an interactive terminal; scripts use `px proxies`,
`px history`, `px stats` and friends.

```sh
px tui                                     # dashboard (tui.start_tab)
px ui --tab proxies --status alive --sort last_ms
px tui -t history
px tui -S 185.199 --dead                   # initial search, dead rows shown
px tui --theme neon --refresh 2s
px --no-color tui --no-mouse --no-alt-screen
```

## Layout

```
 ◆ SilkRouter  1 Dashboard  2 Proxies  3 History  4 Tags  5 Scans  6 Lists  7 Config
 ...the active tab...
 PROXIES  1-500 of 13,201 (page 1/27) · status:alive · sort:rank ↑ · /185.199   ✓ toast
 ↑↓ move · enter details · / search · s/r sort · a/p/D filter · : commands · ? help
```

The status bar shows the rows on screen, the filters and the sort (the arrow
is the effective direction), background jobs with a spinner, and toasts
(results and errors). The bottom line shows the main keys, or the input that
is open (search, tag prompt, config edit, palette).

## Tabs

| # | Tab | What it shows |
|---|-----|---------------|
| 1 | Dashboard | proxies by status and scheme, alive latency buckets, best / median, facts, tags, recent scans, top commands of the week, the proxy of this shell (`SILKROUTER_ACTIVE` / `SILKROUTER_PROXY` / `SILKROUTER_SESSION`) |
| 2 | Proxies | the proxy table (columns from `tui.columns`), `tui.page_size` rows per page; Enter opens the details: every field, scores, the HTTPS / TLS verdict (interception and the certificate issuer), recent measurements, reputation verdicts, tags, usage |
| 3 | History | the usage log (time, mode, proxy, exit, took, ms, error, command); `g` cycles statistics grouped by proxy, cmd, err, day, hour, week, month, mode, session, tty, cwd |
| 4 | Tags | every tag with its proxy count and kind (heuristic / manual); Enter shows the proxies carrying it |
| 5 | Scans | scan runs with state and progress; start a scan (quick, full, reputation, geo, protocols) and follow it live |
| 6 | Lists | reputation lists: kind, entries, age, errors |
| 7 | Config | every key: value, origin (default / file / env), default, help |

## Keys

Everywhere: `1`-`7` jump to a tab, `tab` / `shift+tab` cycle, `:` palette,
`?` help (the keys of the current tab), `ctrl+r` / `F5` reload,
`q` / `ctrl+c` quit (asks first while a check or another job runs, when
`tui.confirm_quit` is on; `ctrl+c` twice forces).

Tables: `↑`/`k`, `↓`/`j`, `pgup`/`ctrl+b`, `pgdn`/`ctrl+f`, `ctrl+u` /
`ctrl+d` half pages, `home`, `end`/`G`; `[` / `]` previous / next page of
rows. Mouse (`tui.mouse`): wheel scrolls, a click selects a row, a second
click opens it, a click on a tab switches to it.

| Tab | Key | Action |
|-----|-----|--------|
| Proxies | `/` | live search (host, URL, exit IP, country, org, source); enter keeps, esc cancels |
| | `s` / `S` | next / previous sort key; `r` reverses |
| | `a` | status filter: all, alive, dead, unknown |
| | `p` | scheme filter: all, http, https, socks, socks4, socks4a, socks5 |
| | `D` | show / hide dead rows while the status filter is all (`tui.show_dead`) |
| | `X` | clear every filter and the search |
| | `enter` | details (`esc` back, `r` reload, arrows scroll) |
| | `t` / `T` | add / remove tags (`TAG` or `TAG:SCOPE`, several at once; tab completes) |
| | `b` | toggle the manual `blocked` tag (route skips blocked proxies) |
| | `c` | check now in the background (`probe.name` or `probe.test_url`, `check.tries`, `check.timeout`); the result is a `probe` measurement and updates the row |
| | `y` | copy the URL with OSC 52 (works over SSH; tmux / screen wrapped) |
| | `F` | full-scan the selected proxy in the background (see Scans) |
| History | `g` | list → stats by proxy → cmd → err → day → ... (`esc` back to the list) |
| | `s` | stats order: uses, last, fail, rate, key |
| | `f` / `m` | result filter (ok, fail, proxy errors) / mode filter (route, shell, relay) |
| | `/`, `X` | search the command line, cwd and proxy / clear the filters |
| | `enter` | list: open the row's proxy · stats: narrow the list to the group |
| Tags | `enter` | the proxies carrying the tag (Proxies tab, every status) |
| | `R` | recompute every heuristic tag now (`tags.*` thresholds) |
| | `/` | filter tag names |
| Scans | `n` | new scan: the setup form (below) |
| | `x` | pause the running scan (asks; resumable) |
| | `esc` | hide the finished scan's panel |
| | `k` | recompute scores and ranks (`scan.rank`), then the tags, like `px rank` (`↑` moves up here) |
| Lists | `u` / `U` | download stale / all lists in the background |
| Config | `enter` / `e` | edit the value in place: validated as you type, written like `px config set` |
| | `x` | reset the key to its default (`px config reset KEY`) |
| | `/` | filter by key or help text |

Edits rewrite `config.toml` in its canonical commented form and keep the
previous file as `config.toml.bak`. `[tui]` keys apply immediately (theme,
columns, page size, refresh, mouse, alternate screen, dead rows, markdown);
a note appears when an environment variable overrides the edited key.

## Scanning from the TUI

Scans started here are the same runs as `px scan MODE`, with every knob at
its config default (`scan.*`, `tags.auto`): recorded in the runs table,
resumable, ranked and tagged at the end. One scan runs at a time.

1. **Start.** `n` in the Scans tab opens the form:

   ```
    New scan
    ▸ mode       ‹ quick ›       one probe per proxy: alive or dead, latency
      selection  ‹ filter ›      what the Proxies tab shows (its filter and search)
      restart    ‹ no ›          resume a paused run of the same mode and selection
      targets    1,234 proxies · the proxy filter (status:alive · /185.199)
   ```

   `↑`/`↓` (or `tab`) choose the field, `←`/`→` (or space) change it,
   `1`-`5` pick the mode (quick, full, reputation, geo, protocols),
   `a` / `A` / `f` / `s` the selection (every proxy, the alive ones, the
   Proxies tab's filter, the proxy selected there), `x` toggles restart,
   `enter` starts, `esc` cancels. The form counts the targets and warns
   when a paused run of that mode exists (it resumes when the selection is
   the same, unless restart is on). Shortcuts: `:scan full alive`,
   `:scan quick filter restart`, and `F` in the Proxies tab (full scan of
   the selected proxy, from the table or its details).
2. **Follow.** The panel above the runs table shows the mode, the
   selection and the state (starting, running, pausing, done, paused),
   a progress bar with done / total, the rate and the ETA, the alive /
   dead / listed counts (and proxies that intercept TLS), the last proxy,
   the scan's messages (`lists: 20/20 loaded`, ...), and the alive proxies
   as they are found (latency, country, anonymity, listings, TLS). The run's
   row in the table advances with it; the status bar shows the scan on
   every tab.
3. **Pause.** `x` (or `:scan stop`) asks, then pauses: the proxies in
   flight finish, everything measured is kept, and the same scan started
   again resumes where it stopped. Quitting while a scan runs asks first
   (`tui.confirm_quit`) and pauses it the same way before exiting.
4. **Result.** A toast sums it up (`full scan #12 done: 1,234/1,234 in 41m
   · 312 alive · 922 dead · 41 listed · ranked 312`); the Proxies, Tags and
   Dashboard tabs reload. `esc` hides the finished panel.

### TLS interception

A full scan's HTTPS check marks a proxy that presents a certificate that
does not verify: it decrypts your HTTPS. Such a proxy gets the heuristic
`mitm` tag, drawn in red everywhere, and route skips it by default
(`route.exclude_tags = blocked, mitm`). The details show `https / tls:
INTERCEPTED` with the certificate's issuer (or `ok`, `failed`, `not
checked`); add `tls` to `tui.columns` for a TLS column (`MITM`, `ok`,
`fail`, `-`).

## Command palette

`:` opens it; `tab` / `shift+tab` complete command names and arguments
(tab names, sort keys, filter keys and values, tag names, config keys and
choices), `↑` / `↓` recall earlier lines, `esc` closes. Commands may be
abbreviated to a unique prefix.

| Command | Action |
|---------|--------|
| `:q`, `:q!` | quit (`:q!` without asking) |
| `:tab NAME` | switch tab (name, prefix or 1-7) |
| `:sort KEY [asc\|desc]` | sort the proxy table by any `px proxies --sort` key |
| `:filter KEY=VALUE ...` | `status`, `scheme`, `tag`, `max_ms`, `min_ms`, `country`, `listed_max`, `min_streak`, `source`, `anon`, `https`, `dead`, `search`; `:filter` alone (or `clear`) resets |
| `:search TEXT` | search the current tab (proxies, history, tags, config) |
| `:tag add NAME...`, `:tag rm NAME...` | tag / untag the selected proxy |
| `:block`, `:unblock` | set / clear the `blocked` tag |
| `:check` | check the selected proxy now |
| `:copy` | copy its URL (OSC 52) |
| `:show ID\|URL\|HOST[:PORT]` | open a proxy's details |
| `:scan MODE [all\|alive\|filter\|selected] [restart]` | start a scan (default: every proxy); `:scan` alone opens the form |
| `:scan stop` | pause the running scan (asks) |
| `:rank [MODE]` | recompute ranks (balanced, speed, clean, stable) and tags |
| `:retag` | recompute the heuristic tags |
| `:refresh` | reload the current tab |
| `:export FILE` | write the URLs of every proxy matching the proxy filter (all pages) |
| `:theme NAME` | silk, mono or neon for this session (`:set tui.theme NAME` keeps it) |
| `:set KEY VALUE` | write a config key (like `px config set`) |
| `:help` | the help overlay |

Example: `:filter status=alive scheme=socks tag=fast max_ms=500`, then
`:export ~/fast-socks.txt`.

## Configuration (`[tui]`)

| Key | Default | Meaning |
|-----|---------|---------|
| `theme` | `silk` | `silk`, `mono` or `neon` (`--theme`) |
| `refresh` | `5s` | reload the visible tab this often, `0` = manual (`--refresh`) |
| `page_size` | `500` | rows loaded per page in tables |
| `start_tab` | `dashboard` | tab shown on start (`--tab`) |
| `mouse` | `true` | mouse support (`--no-mouse`) |
| `alt_screen` | `true` | alternate screen (`--no-alt-screen`) |
| `columns` | `id,proxy,status,last,avg,ok,fail,streak,cc,anon,bl,rank,tags` | proxy table columns; also available: `best`, `rate`, `checked`, `scheme`, `host`, `port`, `speed`, `clean`, `stable`, `uses`, `used`, `exit`, `org`, `source`, `tls` |
| `confirm_quit` | `true` | ask before quitting while a check or a scan runs |
| `markdown` | `true` | render the help with glamour (plain text otherwise) |
| `default_sort` | `rank` | initial sort (`--sort`) |
| `show_dead` | `false` | show dead proxies by default (`--dead`) |

`--no-color` / `NO_COLOR` (or `ui.color = never`) draw the interface
without any escape sequence: the selected row is marked with `▸` and the
active tab with brackets.
