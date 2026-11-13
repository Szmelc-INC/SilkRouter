// Package cli is SilkRouter's command registry, dispatcher, help renderer and
// shell-completion generator.
//
// Commands live in cmd_*.go files in package main and register themselves
// from init():
//
//	func init() {
//		cli.Register(cli.Command{
//			Name:    "list",
//			Aliases: []string{"ls"},
//			Group:   cli.GroupCache,
//			Summary: "print a report of the current cache",
//			Flags: func(fs *cli.FlagSet) {
//				fs.String(&listOpts.cache, "f,cache", fs.Config.CacheFile(), "proxy cache file").
//					Placeholder("FILE").Files().ConfigKey("cache.file")
//			},
//			Run: runList,
//		})
//	}
//
// Help text and zsh/bash/fish completion scripts are generated from this
// metadata, so a registered command is documented and completable for free.
// Flags are parsed with the standard library flag package: every name works
// as both -name and --name.
package cli

import (
	"fmt"
	"sort"
	"strings"
)

// Well-known command groups, in the order the help screen lists them. Later
// features use these names so the overview stays stable; groups without
// commands are not shown.
const (
	GroupRoute   = "Routing"
	GroupCache   = "Cache"
	GroupDB      = "Database"
	GroupScan    = "Scanning"
	GroupCheck   = "Checks"
	GroupHistory = "History & tags"
	GroupShell   = "Shell"
	GroupTUI     = "Interface"
	GroupConfig  = "Configuration"
	GroupMeta    = "Help & info"
)

// DefaultGroupOrder is the order groups appear in the help overview. Groups
// not listed here follow alphabetically.
var DefaultGroupOrder = []string{
	GroupRoute, GroupCache, GroupDB, GroupScan, GroupCheck, GroupHistory,
	GroupShell, GroupTUI, GroupConfig, GroupMeta,
}

// Example is one usage example shown in help. Cmd is written without the
// program name ("build -probe google"); help prefixes the invoked name.
type Example struct {
	Cmd  string
	Desc string
}

// Command is one (sub)command.
type Command struct {
	// Name is the canonical name; Aliases are accepted everywhere Name is.
	Name    string
	Aliases []string
	// Group places the command in the help overview (see Group* constants).
	Group string
	// Summary is a one-line description (lower-case, no trailing period).
	Summary string
	// Description is the long help text; blank lines separate paragraphs.
	Description string
	// Usage is the argument synopsis after the command path, e.g.
	// "[options] [--] <command...>". Defaults to "[options]".
	Usage string
	// Flags defines the command's flags. It must only declare flags (it runs
	// for help and completion too). fs.Config holds the loaded config, so
	// defaults can come from it.
	Flags func(fs *FlagSet)
	// Args describes how positional arguments complete.
	Args Completion
	// Examples are shown at the end of the command's help.
	Examples []Example
	// Run executes the command and returns the process exit code. It is nil
	// for pure command groups (Subcommands only), which print their help.
	Run func(ctx *Context, args []string) int
	// Subcommands nest under this command ("config set").
	Subcommands []Command
	// PassThrough stops flag parsing at the first positional argument and
	// passes everything after it verbatim (route). Other commands accept
	// flags anywhere ("check PROXY -t 5"); "--" ends flags everywhere.
	PassThrough bool
	// NeedsHome creates the config home (paths.Ensure) and runs the
	// first-run legacy import before Run. Commands without it must not
	// write anything under the config home.
	NeedsHome bool
	// Hidden commands work but are left out of help and completion.
	Hidden bool
}

// Names returns the canonical name followed by the aliases.
func (c *Command) Names() []string { return append([]string{c.Name}, c.Aliases...) }

// Has reports whether name is c's name or one of its aliases.
func (c *Command) Has(name string) bool {
	for _, n := range c.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// Sub finds a direct subcommand by name or alias.
func (c *Command) Sub(name string) *Command {
	for i := range c.Subcommands {
		if c.Subcommands[i].Has(name) {
			return &c.Subcommands[i]
		}
	}
	return nil
}

// visibleSubs returns the non-hidden subcommands.
func (c *Command) visibleSubs() []*Command {
	var out []*Command
	for i := range c.Subcommands {
		if !c.Subcommands[i].Hidden {
			out = append(out, &c.Subcommands[i])
		}
	}
	return out
}

// Registry holds the registered commands.
type Registry struct {
	commands []*Command
	// GroupOrder orders the help overview (DefaultGroupOrder by default).
	GroupOrder []string
	// Fallback is the command that receives an invocation whose first
	// argument is not a known command (legacy px.sh style). Empty disables
	// the fallthrough.
	Fallback string
	// LegacyModes maps px.sh-style leading mode flags to commands, e.g.
	// "-B" -> "build". A flag spelled "--x=V" is looked up as "--x" and
	// forwarded as its LegacyMode.Forward flag with value V.
	LegacyModes map[string]LegacyMode
}

// LegacyMode maps one px.sh mode flag to a command.
type LegacyMode struct {
	Command string
	// Forward, when set, is passed to the command in place of the mode flag
	// (keeping its value): "-F FILE" -> "build -F FILE".
	Forward string
	// TakesValue marks a mode flag that consumes the next argument.
	TakesValue bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{GroupOrder: DefaultGroupOrder, LegacyModes: map[string]LegacyMode{}}
}

// Default is the process-wide registry used by Register and Main.
var Default = NewRegistry()

// Register adds cmd to the default registry. It panics on a duplicate name
// or alias, which is a programming error caught by any test run.
func Register(cmd Command) { Default.Register(cmd) }

// Register adds cmd to r.
func (r *Registry) Register(cmd Command) {
	if cmd.Name == "" {
		panic("cli: command without a name")
	}
	for _, n := range cmd.Names() {
		if r.Lookup(n) != nil {
			panic(fmt.Sprintf("cli: command name %q registered twice", n))
		}
	}
	checkSubs(&cmd)
	c := cmd
	r.commands = append(r.commands, &c)
}

func checkSubs(c *Command) {
	seen := map[string]bool{}
	for i := range c.Subcommands {
		s := &c.Subcommands[i]
		for _, n := range s.Names() {
			if seen[n] {
				panic(fmt.Sprintf("cli: subcommand %s %s registered twice", c.Name, n))
			}
			seen[n] = true
		}
		checkSubs(s)
	}
}

// Lookup finds a top-level command by name or alias.
func (r *Registry) Lookup(name string) *Command {
	for _, c := range r.commands {
		if c.Has(name) {
			return c
		}
	}
	return nil
}

// Commands returns every top-level command in registration order.
func (r *Registry) Commands() []*Command { return r.commands }

// Visible returns the non-hidden top-level commands sorted by name.
func (r *Registry) Visible() []*Command {
	var out []*Command
	for _, c := range r.commands {
		if !c.Hidden {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Groups returns the visible commands bucketed by group, in display order.
func (r *Registry) Groups() []GroupCommands {
	byGroup := map[string][]*Command{}
	for _, c := range r.Visible() {
		g := c.Group
		if g == "" {
			g = "Other"
		}
		byGroup[g] = append(byGroup[g], c)
	}
	var out []GroupCommands
	seen := map[string]bool{}
	for _, g := range r.GroupOrder {
		if cmds, ok := byGroup[g]; ok {
			out = append(out, GroupCommands{Name: g, Commands: cmds})
			seen[g] = true
		}
	}
	var rest []string
	for g := range byGroup {
		if !seen[g] {
			rest = append(rest, g)
		}
	}
	sort.Strings(rest)
	for _, g := range rest {
		out = append(out, GroupCommands{Name: g, Commands: byGroup[g]})
	}
	return out
}

// GroupCommands is one group of the help overview.
type GroupCommands struct {
	Name     string
	Commands []*Command
}

// Resolution is the outcome of resolving an argument list to a command.
type Resolution struct {
	// Command is the resolved (leaf or group) command; nil when args were
	// empty.
	Command *Command
	// Path holds the canonical names from the top level down, e.g.
	// ["config", "set"].
	Path []string
	// Args are the arguments for Command.
	Args []string
	// Legacy is set when the invocation fell through to the fallback
	// command or used a px.sh mode flag.
	Legacy bool
}

// PathString joins Path with spaces ("config set").
func (r Resolution) PathString() string { return strings.Join(r.Path, " ") }

// UnknownCommandError reports an unknown subcommand of a command group.
type UnknownCommandError struct {
	Parent  string // command path of the group ("config")
	Name    string
	Suggest []string
}

func (e *UnknownCommandError) Error() string {
	msg := fmt.Sprintf("unknown command %q", e.Name)
	if e.Parent != "" {
		msg = fmt.Sprintf("unknown %s command %q", e.Parent, e.Name)
	}
	if len(e.Suggest) > 0 {
		msg += fmt.Sprintf(" (did you mean %s?)", strings.Join(e.Suggest, " or "))
	}
	return msg
}

// Resolve maps args (global flags already removed) to a command:
//
//   - "" (no args): Resolution{} with a nil Command;
//   - "--" first: the fallback command, with args kept ("--" included) so
//     it never parses the routed program's flags;
//   - a px.sh mode flag first (-B, -R, -L, -F FILE, ...): its command;
//   - a known command name or alias: that command, descending into
//     subcommands while the next argument names one;
//   - anything else: the fallback command with all args (legacy style).
func (r *Registry) Resolve(args []string) (Resolution, error) {
	if len(args) == 0 {
		return Resolution{}, nil
	}
	first := args[0]
	if first == "--" {
		res, err := r.fallback(args)
		res.Legacy = false // an explicit request to route, not a fallthrough
		return res, err
	}
	if res, ok, err := r.legacyMode(args); ok || err != nil {
		return res, err
	}
	cmd := r.Lookup(first)
	if cmd == nil {
		return r.fallback(args)
	}
	res := Resolution{Command: cmd, Path: []string{cmd.Name}, Args: args[1:]}
	for len(cmd.Subcommands) > 0 && len(res.Args) > 0 {
		next := res.Args[0]
		if strings.HasPrefix(next, "-") {
			break
		}
		sub := cmd.Sub(next)
		if sub == nil {
			if cmd.Run != nil {
				break // the group itself takes positional args
			}
			return res, &UnknownCommandError{
				Parent:  res.PathString(),
				Name:    next,
				Suggest: suggest(next, subNames(cmd)),
			}
		}
		cmd = sub
		res.Command = sub
		res.Path = append(res.Path, sub.Name)
		res.Args = res.Args[1:]
	}
	return res, nil
}

func (r *Registry) fallback(args []string) (Resolution, error) {
	if r.Fallback == "" {
		return Resolution{}, &UnknownCommandError{Name: args[0], Suggest: suggest(args[0], r.names())}
	}
	cmd := r.Lookup(r.Fallback)
	if cmd == nil {
		return Resolution{}, fmt.Errorf("cli: fallback command %q is not registered", r.Fallback)
	}
	return Resolution{Command: cmd, Path: []string{cmd.Name}, Args: args, Legacy: true}, nil
}

func (r *Registry) legacyMode(args []string) (Resolution, bool, error) {
	flagName, value, hasValue := strings.Cut(args[0], "=")
	m, ok := r.LegacyModes[flagName]
	if !ok {
		return Resolution{}, false, nil
	}
	cmd := r.Lookup(m.Command)
	if cmd == nil {
		return Resolution{}, false, fmt.Errorf("cli: legacy mode %s maps to unknown command %q", flagName, m.Command)
	}
	rest := args[1:]
	var fwd []string
	switch {
	case m.TakesValue && hasValue:
		fwd = []string{m.Forward, value}
	case m.TakesValue:
		if len(rest) == 0 {
			return Resolution{}, true, fmt.Errorf("%s needs a value", flagName)
		}
		fwd = []string{m.Forward, rest[0]}
		rest = rest[1:]
	case m.Forward != "":
		fwd = []string{m.Forward}
	}
	out := append(fwd, rest...)
	return Resolution{Command: cmd, Path: []string{cmd.Name}, Args: out, Legacy: true}, true, nil
}

func (r *Registry) names() []string {
	var out []string
	for _, c := range r.commands {
		if !c.Hidden {
			out = append(out, c.Names()...)
		}
	}
	return out
}

func subNames(c *Command) []string {
	var out []string
	for _, s := range c.visibleSubs() {
		out = append(out, s.Names()...)
	}
	return out
}

// Suggest returns registered command names close to name (typo help).
func (r *Registry) Suggest(name string) []string { return suggest(name, r.names()) }

// SuggestFrom returns the candidates within a small edit distance of name.
func SuggestFrom(name string, candidates []string) []string { return suggest(name, candidates) }

// suggest returns the candidates within a small edit distance of name.
func suggest(name string, candidates []string) []string {
	best := 3
	if len(name) <= 3 {
		best = 2
	}
	var out []string
	for _, c := range candidates {
		d := levenshtein(name, c)
		switch {
		case d < best:
			best = d
			out = []string{c}
		case d == best && len(out) > 0:
			out = append(out, c)
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
