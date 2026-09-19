// Command silkrouter is a single, dependency-free, cross-platform binary that
// unifies the original SilkRouter bash toolkit:
//
//	build/recache/list  - build & inspect a fast local proxy cache
//	route               - run any command through a cached/random proxy
//	check               - test proxies for liveness/latency
//	blcheck             - check IP(s) against DNS blacklists (mxtoolbox)
//	proxycheck          - check proxy/anonymity detection (whatismyipaddress)
//
// The whole thing is standard library only, so `go build` yields a static
// binary for Linux or Windows with no runtime dependencies.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/signal"

	"github.com/szmelc-inc/silkrouter/internal/source"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

// version is stamped at build time via -ldflags "-X main.version=..."; the
// default keeps `silkrouter version` meaningful for `go build` users too.
var version = "2.0.0-dev"

// builtinList is the consolidated proxy list, embedded so `--source builtin`
// works completely offline.
//
//go:embed proxy/LIST.txt
var builtinList []byte

func main() {
	source.Builtin = builtinList
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	ctx := signalContext()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "route", "run":
		return cmdRoute(ctx, rest)
	case "build":
		return cmdBuild(ctx, rest, false)
	case "recache":
		return cmdBuild(ctx, rest, true)
	case "list", "ls":
		return cmdList(ctx, rest)
	case "check":
		return cmdCheck(ctx, rest)
	case "blcheck":
		return cmdBLCheck(ctx, rest)
	case "proxycheck":
		return cmdProxyCheck(ctx, rest)
	case "probes":
		return cmdProbes(ctx, rest)
	case "version", "-v", "--version":
		fmt.Printf("silkrouter %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		ui.Errf("silkrouter: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		return 2
	}
}

// signalContext returns a context cancelled on the first SIGINT/SIGTERM, so
// long-running builds stop cleanly and keep whatever they gathered so far.
func signalContext() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	// stop is intentionally not deferred: the context lives for the process.
	_ = stop
	return ctx
}

func usage(w *os.File) {
	fmt.Fprintf(w, `silkrouter %s - build a proxy cache and route commands through it

usage:
  silkrouter <command> [options]

commands:
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

Run "silkrouter <command> -h" for command-specific options.

examples:
  silkrouter build -probe cloudflare -j 200
  silkrouter route -- curl -s https://ifconfig.me
  silkrouter route -s -d 500 -- git pull
  silkrouter check socks5://1.2.3.4:1080
  silkrouter blcheck 8.8.8.8
`, version)
}
