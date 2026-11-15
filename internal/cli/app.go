package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

// App is one CLI program built on a Registry.
type App struct {
	Registry *Registry
	// Name is the product name shown in the banner ("SilkRouter").
	Name string
	// Binary is the canonical executable name ("silkrouter").
	Binary string
	// Aliases are the other executable names that behave identically
	// ("px", "s-proxy"); argv[0] picks which one help shows.
	Aliases []string
	Version string
	// Tagline is the one-line description in the banner.
	Tagline string
	// Usage lines of the overview, without the program name.
	Usage []Example
	// Examples of the overview.
	Examples []Example
	// Setup runs after paths.Ensure for commands with NeedsHome (first-run
	// work such as importing a legacy cache).
	Setup func(ctx *Context) error

	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Globals are the flags accepted before any command. --no-color, -q and -v
// are also accepted after the command.
type Globals struct {
	NoColor bool
	Config  string
	Home    string
	Quiet   bool
	Verbose bool
	Version bool
	Help    bool
}

// GlobalFlags declares the global flags on fs, bound to g. With anywhere
// set, only the flags that commands also accept are declared (and only
// those fs does not define itself).
func GlobalFlags(fs *FlagSet, g *Globals, anywhere bool) {
	def := func(names string) bool { return !anywhere || !fs.Has(names) }
	if def("no-color") {
		fs.Bool(&g.NoColor, "no-color", g.NoColor, "disable colored output (also: NO_COLOR=1)").Global = true
	}
	if !anywhere {
		fs.String(&g.Config, "config", g.Config, "config file (env SILKROUTER_CONFIG)").
			Placeholder("FILE").Files().NoDefault()
		fs.String(&g.Home, "home", g.Home, "config home directory (env SILKROUTER_HOME)").
			Placeholder("DIR").Dirs().NoDefault()
	}
	if def("q,quiet") {
		fs.Bool(&g.Quiet, "q,quiet", g.Quiet, "print less (errors only)").Global = true
	}
	if def("v,verbose") {
		fs.Bool(&g.Verbose, "v,verbose", g.Verbose, "print more").Global = true
	}
	if !anywhere {
		fs.Bool(&g.Version, "version", false, "print the version and exit")
		fs.Bool(&g.Help, "h,help", false, "show help (also: <command> --help)")
	}
}

// ParseGlobals consumes leading global flags from args and returns the
// rest. It stops at the first argument that is not a global flag (an
// unknown flag included, so legacy "px -d 500 curl" reaches route), and at
// "--" (which is left in place).
func ParseGlobals(args []string, g *Globals) ([]string, error) {
	fs := NewFlagSet("global", nil)
	GlobalFlags(fs, g, false)
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" || a == "-" || !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "---") {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			break
		}
		if f.IsBool {
			if !hasVal {
				val = "true"
			}
		} else if !hasVal {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag needs an argument: %s", a)
			}
			i++
			val = args[i]
		}
		if err := fs.Std.Set(name, val); err != nil {
			return nil, fmt.Errorf("invalid value %q for flag %s: %v", val, a, err)
		}
		i++
	}
	return args[i:], nil
}

// ProgName maps argv[0] to the name help shows: the executable's base name
// without ".exe" when it is the binary or one of its aliases, else the
// canonical binary name.
func (a *App) ProgName(argv0 string) string {
	base := filepath.Base(strings.ReplaceAll(argv0, `\`, "/"))
	if strings.EqualFold(filepath.Ext(base), ".exe") {
		base = base[:len(base)-4]
	}
	for _, n := range a.Names() {
		if strings.EqualFold(base, n) {
			return n
		}
	}
	return a.Binary
}

// Names returns the binary name followed by its aliases.
func (a *App) Names() []string { return append([]string{a.Binary}, a.Aliases...) }

func (a *App) streams() (io.Reader, io.Writer, io.Writer) {
	in, out, errw := a.Stdin, a.Stdout, a.Stderr
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if errw == nil {
		errw = os.Stderr
	}
	return in, out, errw
}

// Run executes argv (argv[0] included) and returns the exit code.
func (a *App) Run(parent context.Context, argv []string) int {
	stdin, stdout, stderr := a.streams()
	prog := a.Binary
	if len(argv) > 0 {
		prog = a.ProgName(argv[0])
		argv = argv[1:]
	}
	errorf := func(format string, args ...any) {
		fmt.Fprintf(stderr, "%s: %s\n", prog, fmt.Sprintf(format, args...))
	}

	var g Globals
	rest, err := ParseGlobals(argv, &g)
	if err != nil {
		errorf("%v", err)
		return 2
	}
	if g.Home != "" {
		home := paths.ExpandUser(g.Home)
		if abs, err := filepath.Abs(home); err == nil {
			home = abs
		}
		os.Setenv(paths.EnvHome, home)
	}

	cfgPath := paths.ConfigPath(g.Config)
	cfg, cfgErr := config.Load(cfgPath, os.LookupEnv)
	switch {
	case g.NoColor || ui.NoColorEnv():
		ui.SetMode(ui.ModeNever)
	default:
		ui.SetMode(cfg.UI.Color)
	}

	ctx := &Context{
		Context:    parent,
		App:        a,
		Prog:       prog,
		Config:     cfg,
		ConfigPath: cfgPath,
		ConfigErr:  cfgErr,
		Globals:    &g,
		Stdin:      stdin,
		Stdout:     stdout,
		Stderr:     stderr,
	}

	if g.Version {
		a.WriteVersion(stdout, prog)
		return 0
	}
	res, rerr := a.Registry.Resolve(rest)
	if g.Help {
		if rerr == nil && res.Command != nil && !res.Legacy {
			a.WriteCommandHelp(stdout, prog, res.Path, cfg)
		} else {
			a.WriteHelp(stdout, prog, cfg)
		}
		return 0
	}
	if rerr != nil {
		errorf("%v", rerr)
		hint := prog + " help"
		var uerr *UnknownCommandError
		if errors.As(rerr, &uerr) && uerr.Parent != "" {
			hint = prog + " " + uerr.Parent + " --help"
		}
		fmt.Fprintf(stderr, "Run '%s' for usage.\n", hint)
		return 2
	}
	if res.Command == nil {
		a.WriteHelp(stderr, prog, cfg)
		return 2
	}
	return a.dispatch(ctx, res)
}

func (a *App) dispatch(ctx *Context, res Resolution) int {
	cmd := res.Command
	ctx.Command = cmd
	ctx.Path = res.Path
	ctx.Legacy = res.Legacy

	fs := NewFlagSet(res.PathString(), ctx.Config)
	if cmd.Flags != nil {
		cmd.Flags(fs)
	}
	GlobalFlags(fs, ctx.Globals, true)
	ctx.Flags = fs
	args, err := fs.parse(res.Args, !cmd.PassThrough)
	if errors.Is(err, flag.ErrHelp) {
		a.WriteCommandHelp(ctx.Stdout, ctx.Prog, res.Path, ctx.Config)
		return 0
	}
	if err != nil {
		return ctx.UsageError("%v", err)
	}
	if ctx.Globals.NoColor {
		ui.SetMode(ui.ModeNever)
	}
	ctx.Quiet = ctx.Globals.Quiet
	ctx.Verbose = ctx.Globals.Verbose && !ctx.Quiet

	// Config problems are reported once flags are parsed, so -q works after
	// the command too. Hidden commands serve shell completion: stay silent.
	if !cmd.Hidden && !ctx.Quiet {
		if ctx.ConfigErr != nil {
			ctx.Errorf("warning: %v (using defaults)", ctx.ConfigErr)
		}
		for _, w := range ctx.Config.Warnings() {
			ctx.Errorf("warning: %s", w)
		}
	}

	if cmd.Run == nil {
		a.WriteCommandHelp(ctx.Stdout, ctx.Prog, res.Path, ctx.Config)
		return 0
	}
	if cmd.NeedsHome {
		if err := paths.Ensure(); err != nil {
			ctx.Errorf("cannot create config home: %v", err)
			return 1
		}
		if a.Setup != nil {
			if err := a.Setup(ctx); err != nil {
				ctx.Errorf("warning: %v", err)
			}
		}
	}
	return cmd.Run(ctx, args)
}

// Context is what a command's Run receives. It embeds the cancellation
// context (cancelled on SIGINT), so it can be passed wherever a
// context.Context is expected.
type Context struct {
	context.Context
	App *App
	// Command is the command being run and Path its canonical path.
	Command *Command
	Path    []string
	// Prog is the invoked program name (px, s-proxy or silkrouter).
	Prog string
	// Config is the effective config (defaults < file < environment);
	// ConfigPath is the file it was read from; ConfigErr is a load error
	// (Config then holds defaults + environment).
	Config     *config.Config
	ConfigPath string
	ConfigErr  error
	Globals    *Globals
	// Flags is the parsed flag set (see FlagSet.Changed).
	Flags  *FlagSet
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Quiet and Verbose are the -q / -v global flags (quiet wins).
	Quiet, Verbose bool
	// Legacy is set when the command was reached through the px.sh-style
	// fallthrough or a px.sh mode flag.
	Legacy bool
}

// CommandPath is "prog sub sub" for messages.
func (c *Context) CommandPath() string {
	return strings.Join(append([]string{c.Prog}, c.Path...), " ")
}

// Errorf prints "prog: message" to stderr (always, even with -q).
func (c *Context) Errorf(format string, a ...any) {
	fmt.Fprintf(c.Stderr, "%s: %s\n", c.Prog, fmt.Sprintf(format, a...))
}

// Infof prints "prog: message" to stderr unless -q is set.
func (c *Context) Infof(format string, a ...any) {
	if !c.Quiet {
		c.Errorf(format, a...)
	}
}

// Debugf prints "prog: message" to stderr only with -v.
func (c *Context) Debugf(format string, a ...any) {
	if c.Verbose {
		c.Errorf(format, a...)
	}
}

// UsageError prints the error plus a pointer to the command's help and
// returns exit code 2.
func (c *Context) UsageError(format string, a ...any) int {
	c.Errorf(format, a...)
	fmt.Fprintf(c.Stderr, "Run '%s --help' for usage.\n", c.CommandPath())
	return 2
}

// Colors returns the palette for stdout.
func (c *Context) Colors() ui.Palette { return ui.ColorsFor(c.Stdout) }

// ErrColors returns the palette for stderr.
func (c *Context) ErrColors() ui.Palette { return ui.ColorsFor(c.Stderr) }

// JSON writes v as indented JSON to stdout and returns an exit code.
func (c *Context) JSON(v any) int {
	enc := json.NewEncoder(c.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		c.Errorf("%v", err)
		return 1
	}
	return 0
}

// Help writes the current command's help to stdout and returns 0.
func (c *Context) Help() int {
	c.App.WriteCommandHelp(c.Stdout, c.Prog, c.Path, c.Config)
	return 0
}

// WriteVersion prints the version line.
func (a *App) WriteVersion(w io.Writer, prog string) {
	fmt.Fprintf(w, "%s %s\n", a.Binary, a.Version)
}
