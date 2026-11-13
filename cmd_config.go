package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/paths"
)

var configOpts struct {
	json     bool
	sources  bool
	defaults bool
	force    bool
}

func jsonFlag(fs *cli.FlagSet, what string) {
	fs.Bool(&configOpts.json, "json", false, "print "+what+" as JSON")
}

func init() {
	cli.Register(cli.Command{
		Name:    "config",
		Aliases: []string{"cfg"},
		Group:   cli.GroupConfig,
		Summary: "show and edit the configuration (config.toml)",
		Description: `The configuration lives in config.toml inside the config home (see
'paths'). Precedence: built-in defaults < config.toml < environment
(SILKROUTER_<TABLE>_<KEY>, legacy PX_LIST and MXTB_TOKEN) < command-line
flags. Every key has a documented default; 'config keys' lists them.

'set', 'reset' and 'init' rewrite the file in a canonical, commented form
(the previous file is kept as config.toml.bak).`,
		Subcommands: []cli.Command{
			{
				Name:    "path",
				Summary: "print the config file path",
				Run: func(ctx *cli.Context, _ []string) int {
					fmt.Fprintln(ctx.Stdout, ctx.ConfigPath)
					return 0
				},
			},
			{
				Name:    "show",
				Aliases: []string{"cat"},
				Summary: "print the effective configuration",
				Description: `Prints every key with its effective value (defaults < file <
environment) as TOML, or as JSON with --json.`,
				Flags: func(fs *cli.FlagSet) {
					jsonFlag(fs, "the configuration")
					fs.Bool(&configOpts.sources, "sources", false, "annotate where each value comes from")
					fs.Bool(&configOpts.defaults, "defaults", false, "print the built-in defaults instead")
				},
				Run: runConfigShow,
			},
			{
				Name:    "get",
				Summary: "print one value",
				Usage:   "[options] <key>",
				Args:    cli.ArgsDynamic("config-keys"),
				Flags:   func(fs *cli.FlagSet) { jsonFlag(fs, "the value") },
				Examples: []cli.Example{
					{Cmd: "config get route.max_ms"},
				},
				Run: runConfigGet,
			},
			{
				Name:      "set",
				Summary:   "set one value in config.toml",
				Usage:     "<key> <value>",
				Args:      cli.ArgsDynamic("config-keys"),
				NeedsHome: true,
				Examples: []cli.Example{
					{Cmd: "config set route.max_ms 500"},
					{Cmd: "config set build.timeout 10s"},
					{Cmd: "config set ui.color never"},
				},
				Run: runConfigSet,
			},
			{
				Name:      "reset",
				Aliases:   []string{"unset"},
				Summary:   "reset keys (or the whole file) to the defaults",
				Usage:     "[<key>...]",
				Args:      cli.Completion{Kind: cli.CompleteDynamic, Topic: "config-keys", Repeat: true},
				NeedsHome: true,
				Examples: []cli.Example{
					{Cmd: "config reset route.max_ms", Desc: "one key back to its default"},
					{Cmd: "config reset", Desc: "the whole file (a .bak copy is kept)"},
				},
				Run: runConfigReset,
			},
			{
				Name:      "edit",
				Summary:   "open config.toml in $VISUAL / $EDITOR",
				NeedsHome: true,
				Run:       runConfigEdit,
			},
			{
				Name:      "init",
				Summary:   "write a commented default config.toml",
				NeedsHome: true,
				Flags: func(fs *cli.FlagSet) {
					fs.Bool(&configOpts.force, "f,force", false, "overwrite an existing file (keeps a .bak copy)")
				},
				Run: runConfigInit,
			},
			{
				Name:    "keys",
				Summary: "list every key with its type, default and environment variable",
				Flags:   func(fs *cli.FlagSet) { jsonFlag(fs, "the keys") },
				Run:     runConfigKeys,
			},
		},
		Examples: []cli.Example{
			{Cmd: "config init"},
			{Cmd: "config set route.max_ms 500"},
			{Cmd: "config show --sources"},
		},
	})

	cli.Register(cli.Command{
		Name:    "paths",
		Aliases: []string{"home"},
		Group:   cli.GroupConfig,
		Summary: "print the config home and every file in it",
		Description: `Prints the resolved layout of the config home: config file, database,
lists, logs, shell snippets, runtime state and the legacy text cache.
With a NAME, prints only that path (handy in scripts: cd "$(px paths home)").

The home is $SILKROUTER_HOME (or --home), else
${XDG_CONFIG_HOME:-~/.config}/szmelc/proxy.`,
		Usage: "[options] [name]",
		Args:  cli.ArgsChoices("home", "config", "db", "lists", "logs", "shell", "run", "cache"),
		Flags: func(fs *cli.FlagSet) { jsonFlag(fs, "the layout") },
		Run:   runPaths,
	})

	cli.RegisterCompleter("config-keys", func(*cli.Context) []cli.Candidate {
		var out []cli.Candidate
		for _, k := range config.Keys() {
			out = append(out, cli.Candidate{Value: k.Name, Desc: k.Help})
		}
		return out
	})
}

func runConfigShow(ctx *cli.Context, _ []string) int {
	cfg := ctx.Config
	if configOpts.defaults {
		cfg = config.Default()
	}
	if configOpts.json {
		if configOpts.sources {
			type row struct {
				Key    string `json:"key"`
				Value  any    `json:"value"`
				Source string `json:"source"`
			}
			var out []row
			for _, k := range config.Keys() {
				v, _ := cfg.Value(k.Name)
				out = append(out, row{Key: k.Name, Value: v, Source: cfg.Origin(k.Name)})
			}
			return ctx.JSON(out)
		}
		return ctx.JSON(cfg.Map())
	}
	fmt.Fprintf(ctx.Stdout, "# %s\n", ctx.ConfigPath)
	if err := config.Write(ctx.Stdout, cfg, config.WriteOptions{Sources: configOpts.sources}); err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	return 0
}

func unknownKey(ctx *cli.Context, key string) int {
	msg := fmt.Sprintf("unknown config key %q", key)
	near := cli.SuggestFrom(key, config.KeyNames())
	if len(near) == 0 {
		for _, k := range config.KeyNames() {
			if strings.HasSuffix(k, "."+key) || strings.Contains(k, key) {
				near = append(near, k)
			}
		}
	}
	if len(near) > 0 && len(near) <= 5 {
		msg += " (did you mean " + strings.Join(near, ", ") + "?)"
	}
	ctx.Errorf("%s", msg)
	fmt.Fprintf(ctx.Stderr, "Run '%s config keys' for the list.\n", ctx.Prog)
	return 2
}

func runConfigGet(ctx *cli.Context, args []string) int {
	if len(args) != 1 {
		return ctx.UsageError("need exactly one key")
	}
	if _, ok := config.Lookup(args[0]); !ok {
		return unknownKey(ctx, args[0])
	}
	if configOpts.json {
		v, _ := ctx.Config.Value(args[0])
		return ctx.JSON(v)
	}
	v, _ := ctx.Config.Get(args[0])
	fmt.Fprintln(ctx.Stdout, v)
	return 0
}

// loadForWrite reads the config file without the environment layer, so
// environment overrides never get baked into the file.
func loadForWrite(ctx *cli.Context) (*config.Config, bool) {
	cfg, err := config.LoadFile(ctx.ConfigPath)
	if err != nil {
		ctx.Errorf("%v", err)
		ctx.Errorf("fix the file first ('%s config edit') or start over ('%s config init --force')", ctx.Prog, ctx.Prog)
		return nil, false
	}
	return cfg, true
}

func saveConfig(ctx *cli.Context, cfg *config.Config) bool {
	if _, err := config.Backup(ctx.ConfigPath); err != nil {
		ctx.Errorf("backup: %v", err)
		return false
	}
	if err := config.Save(ctx.ConfigPath, cfg); err != nil {
		ctx.Errorf("%v", err)
		return false
	}
	return true
}

// warnEnvOverride notes when the environment hides the file's value.
func warnEnvOverride(ctx *cli.Context, key string) {
	if src := ctx.Config.Origin(key); strings.HasPrefix(src, "env ") {
		ctx.Infof("note: $%s overrides this value in the current environment", strings.TrimPrefix(src, "env "))
	}
}

func runConfigSet(ctx *cli.Context, args []string) int {
	if len(args) != 2 {
		return ctx.UsageError("need a key and a value")
	}
	key, value := args[0], args[1]
	if _, ok := config.Lookup(key); !ok {
		return unknownKey(ctx, key)
	}
	cfg, ok := loadForWrite(ctx)
	if !ok {
		return 1
	}
	if err := cfg.Set(key, value); err != nil {
		ctx.Errorf("%v", err)
		return 2
	}
	if !saveConfig(ctx, cfg) {
		return 1
	}
	v, _ := cfg.Get(key)
	ctx.Infof("%s = %s (%s)", key, v, paths.Pretty(ctx.ConfigPath))
	warnEnvOverride(ctx, key)
	return 0
}

func runConfigReset(ctx *cli.Context, args []string) int {
	if len(args) == 0 {
		bak, err := config.Backup(ctx.ConfigPath)
		if err != nil {
			ctx.Errorf("backup: %v", err)
			return 1
		}
		if err := config.WriteDefault(ctx.ConfigPath, true); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
		ctx.Infof("reset %s to defaults", paths.Pretty(ctx.ConfigPath))
		if bak != "" {
			ctx.Infof("previous file saved as %s", paths.Pretty(bak))
		}
		return 0
	}
	for _, k := range args {
		if _, ok := config.Lookup(k); !ok {
			return unknownKey(ctx, k)
		}
	}
	cfg, ok := loadForWrite(ctx)
	if !ok {
		return 1
	}
	for _, k := range args {
		_ = cfg.Reset(k)
	}
	if !saveConfig(ctx, cfg) {
		return 1
	}
	for _, k := range args {
		v, _ := cfg.Get(k)
		ctx.Infof("%s = %s (default)", k, v)
		warnEnvOverride(ctx, k)
	}
	return 0
}

func runConfigInit(ctx *cli.Context, _ []string) int {
	exists := false
	if _, err := os.Stat(ctx.ConfigPath); err == nil {
		exists = true
	}
	if exists && !configOpts.force {
		ctx.Errorf("%s already exists (use --force to overwrite; a .bak copy is kept)", ctx.ConfigPath)
		return 1
	}
	if exists {
		if _, err := config.Backup(ctx.ConfigPath); err != nil {
			ctx.Errorf("backup: %v", err)
			return 1
		}
	}
	if err := config.WriteDefault(ctx.ConfigPath, true); err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	ctx.Infof("wrote %s", paths.Pretty(ctx.ConfigPath))
	return 0
}

func runConfigEdit(ctx *cli.Context, _ []string) int {
	if _, err := os.Stat(ctx.ConfigPath); errors.Is(err, os.ErrNotExist) {
		if err := config.WriteDefault(ctx.ConfigPath, false); err != nil {
			ctx.Errorf("%v", err)
			return 1
		}
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
		if runtime.GOOS == "windows" {
			editor = "notepad"
		}
	}
	argv := append(strings.Fields(editor), ctx.ConfigPath)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		ctx.Errorf("editor %q: %v", editor, err)
		return 1
	}
	cfg, err := config.LoadFile(ctx.ConfigPath)
	if err != nil {
		ctx.Errorf("%v", err)
		return 1
	}
	for _, w := range cfg.Warnings() {
		ctx.Errorf("warning: %s", w)
	}
	return 0
}

func runConfigKeys(ctx *cli.Context, _ []string) int {
	if configOpts.json {
		return ctx.JSON(config.Keys())
	}
	c := ctx.Colors()
	for _, t := range config.Tables() {
		fmt.Fprintf(ctx.Stdout, "%s%s[%s]%s %s%s%s\n", c.Bold, c.Yellow, t.Name, c.Reset, c.Gray, t.Help, c.Reset)
		if len(t.Keys) == 0 {
			fmt.Fprintf(ctx.Stdout, "  %s(no settings yet)%s\n", c.Gray, c.Reset)
		}
		width := 0
		for _, k := range t.Keys {
			width = max(width, len(k.Name))
		}
		for _, k := range t.Keys {
			def := k.Default
			if def == "" {
				def = `""`
			}
			fmt.Fprintf(ctx.Stdout, "  %s%-*s%s  %-8s %s%s%s\n", c.Cyan, width, k.Name, c.Reset, k.Type, c.Green, def, c.Reset)
			fmt.Fprintf(ctx.Stdout, "  %*s  %s", width, "", k.Help)
			if len(k.Choices) > 0 {
				fmt.Fprintf(ctx.Stdout, " (one of: %s)", strings.Join(k.Choices, ", "))
			}
			fmt.Fprintf(ctx.Stdout, " %s[%s]%s\n", c.Gray, strings.Join(k.Env, ", "), c.Reset)
		}
		fmt.Fprintln(ctx.Stdout)
	}
	return 0
}

func runPaths(ctx *cli.Context, args []string) int {
	rows := paths.Layout(ctx.ConfigPath)
	if len(args) > 1 {
		return ctx.UsageError("at most one name")
	}
	if len(args) == 1 {
		for _, r := range rows {
			if r.Name == args[0] {
				if configOpts.json {
					return ctx.JSON(r)
				}
				fmt.Fprintln(ctx.Stdout, r.Path)
				return 0
			}
		}
		return ctx.UsageError("unknown path %q (want one of: home, config, db, lists, logs, shell, run, cache)", args[0])
	}
	if configOpts.json {
		return ctx.JSON(rows)
	}
	c := ctx.Colors()
	nw, pw := 0, 0
	for _, r := range rows {
		nw = max(nw, len(r.Name))
		pw = max(pw, len(paths.Pretty(r.Path)))
	}
	for _, r := range rows {
		mark := c.Green + "✓" + c.Reset
		if !r.Exists {
			mark = c.Gray + "·" + c.Reset
		}
		fmt.Fprintf(ctx.Stdout, "%s %s%-*s%s  %-*s  %s%s%s\n", mark, c.Cyan, nw, r.Name, c.Reset, pw, paths.Pretty(r.Path), c.Gray, r.Desc, c.Reset)
	}
	return 0
}
