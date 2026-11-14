// Command silkrouter builds a fast local proxy cache and routes any command
// through it. The same binary answers to px, s-proxy and silkrouter.
//
// main only boots the command registry: every command lives in its own
// cmd_*.go file and registers itself from init() (see internal/cli).
package main

import (
	"context"
	_ "embed"
	"os"
	"os/signal"
	"syscall"

	"github.com/szmelc-inc/silkrouter/internal/cache"
	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/source"
)

// version is stamped at build time via -ldflags "-X main.version=..."; the
// default keeps `silkrouter version` meaningful for `go build` users too.
var version = "4.0.0-dev"

// builtinList is the consolidated proxy list, embedded so `-builtin` works
// completely offline.
//
//go:embed proxy/LIST.txt
var builtinList []byte

func main() {
	source.Builtin = builtinList
	os.Exit(newApp().Run(signalContext(), os.Args))
}

// newApp wires the default registry into the program: names, the legacy
// px.sh fallthrough and the first-run hook.
func newApp() *cli.App {
	reg := cli.Default
	reg.Fallback = "route"
	build := cli.LegacyMode{Command: "build"}
	fromFile := cli.LegacyMode{Command: "build", Forward: "-F", TakesValue: true}
	reg.LegacyModes = map[string]cli.LegacyMode{
		"-B": build, "--build": build,
		"-R": {Command: "recache"}, "--recache": {Command: "recache"},
		"-L": {Command: "list"}, "--list": {Command: "list"},
		"-F": fromFile, "--file": fromFile,
		"--fresh": {Command: "route", Forward: "-r"},
	}
	return &cli.App{
		Registry: reg,
		Name:     "SilkRouter",
		Binary:   "silkrouter",
		Aliases:  []string{"px", "s-proxy"},
		Version:  version,
		Tagline:  "route any command (or a whole shell) through fast, scanned, ranked proxies",
		Usage: []cli.Example{
			{Cmd: "[global options] <command> [options] [args]"},
			{Cmd: "[route options] <program> [args...]", Desc: "legacy px.sh style: same as route"},
			{Cmd: "-- <program> [args...]", Desc: "always route (even a subcommand's name)"},
			{Cmd: "-B | -R | -L | -F FILE", Desc: "px.sh modes: build, recache, list, build -F"},
		},
		Examples: []cli.Example{
			{Cmd: "build -probe cloudflare -j 200", Desc: "build a tested, sorted cache"},
			{Cmd: "curl -s https://ifconfig.me", Desc: "route a command through a random cached proxy"},
			{Cmd: "-s -d 500 git pull", Desc: "socks only, under 500 ms"},
			{Cmd: "config set route.max_ms 500", Desc: "change a default"},
			{Cmd: "completion zsh > ~/.zfunc/_silkrouter", Desc: "install shell completion"},
		},
		Setup: firstRun,
	}
}

// firstRun imports a pre-v4 text cache ($PX_LIST or the v3 temp file) into
// <home>/cache.list the first time it is missing. The database import of
// that file happens in the db layer.
func firstRun(ctx *cli.Context) error {
	src, n, err := cache.ImportLegacy(paths.LegacyCache(), paths.LegacyCandidates())
	if err != nil {
		return err
	}
	if src != "" {
		ctx.Infof("imported legacy cache %s (%d proxies) -> %s", src, n, paths.Pretty(paths.LegacyCache()))
	}
	return nil
}

// signalContext returns a context cancelled on the first SIGINT/SIGTERM, so
// long-running builds stop cleanly and keep whatever they gathered so far.
func signalContext() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// stop is intentionally not deferred: the context lives for the process.
	_ = stop
	return ctx
}
