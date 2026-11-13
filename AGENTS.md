# Working on SilkRouter

Read `docs/ARCHITECTURE-v4.md` before changing anything; it is the binding
contract for paths, config, CLI, database, shell and testing.

## Rules

- Code, comments and docs in English.
- Never use `sudo`. If something needs elevated privileges, use `pkexec`.
  README install instructions must not tell users to `sudo`.
- Every CLI surface is rich: many subcommands and flags, config-file keys for
  every tunable, colored and aligned `--help`, `--no-color`/`NO_COLOR`,
  `--json` for data output, zsh/bash/fish completion generated from the
  command registry.
- The TUI is written in Go (bubbletea/lipgloss/bubbles/glamour), modeled on
  `~/Projects-4/ULTIMATE-TUI`: fully interactive, modular, keybindings, `:`
  command palette, help overlay, rich colors and pragmatic markdown.
- Packaging is part of the project: keep `packaging/arch/PKGBUILD` and
  `flake.nix` building after every change that affects build or install.
- Keep `README.md` truthful about features and dependencies.
- `CGO_ENABLED=0` builds and `make release` cross-compiles must keep working.
- `make check` must pass before committing.

## Layout

```
main.go            boot: argv[0] dispatch + command registry
cmd_*.go           one file per command group, registered via init()
internal/paths     config home resolution
internal/config    TOML config, defaults, env overrides
internal/cli       command registry, help renderer, completions
internal/db        SQLite schema, migrations, queries
internal/proxy     protocol dialers (http/https/socks4/4a/5)
internal/probe     validation probes
internal/source    candidate lists
internal/cache     legacy text cache
internal/router    run a command through a proxy
internal/scan      blacklist / anonymity checkers
internal/ui        colors and terminal helpers
```
