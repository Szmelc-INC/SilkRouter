package main

import (
	"fmt"
	"runtime"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/paths"
)

var versionOpts struct{ json bool }

func init() {
	cli.Register(cli.Command{
		Name:    "help",
		Group:   cli.GroupMeta,
		Summary: "show help for the program or a command",
		Usage:   "[command [subcommand]]",
		Args:    cli.Completion{Kind: cli.CompleteDynamic, Topic: "commands"},
		Examples: []cli.Example{
			{Cmd: "help route"},
			{Cmd: "help config set"},
		},
		Run: func(ctx *cli.Context, args []string) int {
			if len(args) == 0 {
				ctx.App.WriteHelp(ctx.Stdout, ctx.Prog, ctx.Config)
				return 0
			}
			path, err := ctx.App.ResolveHelpPath(args)
			if err != nil {
				ctx.Errorf("%v", err)
				return 2
			}
			ctx.App.WriteCommandHelp(ctx.Stdout, ctx.Prog, path, ctx.Config)
			return 0
		},
	})

	cli.Register(cli.Command{
		Name:    "version",
		Group:   cli.GroupMeta,
		Summary: "print the version",
		Flags: func(fs *cli.FlagSet) {
			fs.Bool(&versionOpts.json, "json", false, "print version details as JSON")
		},
		Run: func(ctx *cli.Context, _ []string) int {
			info := struct {
				Version string `json:"version"`
				Go      string `json:"go"`
				OS      string `json:"os"`
				Arch    string `json:"arch"`
				Home    string `json:"home"`
				Config  string `json:"config"`
			}{version, runtime.Version(), runtime.GOOS, runtime.GOARCH, paths.Home(), ctx.ConfigPath}
			if versionOpts.json {
				return ctx.JSON(info)
			}
			ctx.App.WriteVersion(ctx.Stdout, ctx.Prog)
			if ctx.Verbose {
				fmt.Fprintf(ctx.Stdout, "go:     %s %s/%s\nhome:   %s\nconfig: %s\n",
					info.Go, info.OS, info.Arch, info.Home, info.Config)
			}
			return 0
		},
	})

	cli.Register(cli.Command{
		Name:    "completion",
		Group:   cli.GroupMeta,
		Summary: "print a shell completion script (zsh, bash, fish)",
		Description: `Prints a completion script generated from the command registry. It
completes for all three names (px, s-proxy, silkrouter).

  zsh:   source <(px completion zsh)
         or save it as _silkrouter in a directory on $fpath
  bash:  source <(px completion bash)
         or save it to ~/.local/share/bash-completion/completions/px
  fish:  px completion fish | source
         or save it to ~/.config/fish/completions/px.fish

'make install' installs all three under $(PREFIX)/share.`,
		Usage: "<zsh | bash | fish>",
		Args:  cli.ArgsChoices(cli.Shells...),
		Run: func(ctx *cli.Context, args []string) int {
			if len(args) != 1 {
				return ctx.UsageError("need a shell: zsh, bash or fish")
			}
			if err := ctx.App.WriteCompletion(ctx.Stdout, args[0]); err != nil {
				ctx.Errorf("%v", err)
				return 2
			}
			return 0
		},
	})

	cli.Register(cli.Command{
		Name:    "__complete",
		Hidden:  true,
		Summary: "print dynamic completion values (used by completion scripts)",
		Usage:   "<topic>",
		Run: func(ctx *cli.Context, args []string) int {
			if len(args) != 1 {
				ctx.Errorf("topics: %v", cli.CompleterTopics())
				return 2
			}
			cands, err := ctx.App.Complete(ctx, args[0])
			if err != nil {
				ctx.Errorf("%v", err)
				return 2
			}
			cli.WriteCandidates(ctx.Stdout, cands)
			return 0
		},
	})
}
