package cli

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/szmelc-inc/silkrouter/internal/config"
)

// Shells lists the shells `completion` can generate scripts for.
var Shells = []string{"zsh", "bash", "fish"}

// Candidate is one dynamic completion value.
type Candidate struct {
	Value string
	Desc  string
}

// Completer produces dynamic completion values for a topic.
type Completer func(ctx *Context) []Candidate

var (
	completersMu sync.Mutex
	completers   = map[string]Completer{}
)

// RegisterCompleter registers the dynamic completion topic answered by
// `prog __complete <topic>` (used by Flag.Dynamic and ArgsDynamic).
func RegisterCompleter(topic string, fn Completer) {
	completersMu.Lock()
	defer completersMu.Unlock()
	if _, dup := completers[topic]; dup {
		panic("cli: completer " + topic + " registered twice")
	}
	completers[topic] = fn
}

// CompleterTopics returns the registered dynamic topics, sorted.
func CompleterTopics() []string {
	completersMu.Lock()
	defer completersMu.Unlock()
	out := []string{"commands"}
	for t := range completers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Complete answers a dynamic completion topic. The built-in "commands"
// topic lists the visible top-level commands.
func (a *App) Complete(ctx *Context, topic string) ([]Candidate, error) {
	if topic == "commands" {
		var out []Candidate
		for _, c := range a.Registry.Visible() {
			for _, n := range c.Names() {
				out = append(out, Candidate{Value: n, Desc: c.Summary})
			}
		}
		return out, nil
	}
	completersMu.Lock()
	fn, ok := completers[topic]
	completersMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown completion topic %q (have: %s)", topic, strings.Join(CompleterTopics(), ", "))
	}
	return fn(ctx), nil
}

// WriteCandidates prints candidates as "value<TAB>description" lines, the
// format the generated scripts read.
func WriteCandidates(w io.Writer, cands []Candidate) {
	for _, c := range cands {
		desc := strings.ReplaceAll(strings.ReplaceAll(c.Desc, "\t", " "), "\n", " ")
		if desc == "" {
			fmt.Fprintln(w, c.Value)
		} else {
			fmt.Fprintf(w, "%s\t%s\n", c.Value, desc)
		}
	}
}

// node is one level of the completion tree.
type node struct {
	path        []string // canonical, empty for the root
	names       []string // name + aliases
	summary     string
	flags       []*Flag
	subs        []*node
	args        Completion
	maxArgs     int
	passThrough bool
}

func (n *node) key() string { return strings.Join(n.path, "/") }

// fn is a shell-safe identifier for the node ("config_set").
func (n *node) fn() string {
	if len(n.path) == 0 {
		return "root"
	}
	return identRe.ReplaceAllString(strings.Join(n.path, "_"), "_")
}

var identRe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// walk visits n and every descendant, parents first.
func (n *node) walk(fn func(*node)) {
	fn(n)
	for _, s := range n.subs {
		s.walk(fn)
	}
}

// tree builds the completion tree from the registry. Flag defaults come
// from the built-in config so scripts do not depend on the user's file.
func (a *App) tree() *node {
	cfg := config.Default()
	root := &node{}
	gfs := NewFlagSet("global", cfg)
	GlobalFlags(gfs, &Globals{}, false)
	root.flags = gfs.Visible()
	for _, c := range a.Registry.Visible() {
		root.subs = append(root.subs, buildNode(c, nil, cfg))
	}
	return root
}

func buildNode(c *Command, parent []string, cfg *config.Config) *node {
	n := &node{
		path:        append(append([]string(nil), parent...), c.Name),
		names:       c.Names(),
		summary:     c.Summary,
		args:        c.Args,
		passThrough: c.PassThrough,
	}
	fs := NewFlagSet(c.Name, cfg)
	if c.Flags != nil {
		c.Flags(fs)
	}
	GlobalFlags(fs, &Globals{}, true)
	n.flags = append(fs.Visible(), &Flag{Names: []string{"h", "help"}, Usage: "show help", IsBool: true})
	for _, s := range c.visibleSubs() {
		n.subs = append(n.subs, buildNode(s, n.path, cfg))
	}
	if (c.Args.Kind == CompleteChoices || c.Args.Kind == CompleteDynamic) && !c.Args.Repeat {
		n.maxArgs = 1
	}
	return n
}

// WriteCompletion writes the completion script for shell.
func (a *App) WriteCompletion(w io.Writer, shell string) error {
	t := a.tree()
	switch shell {
	case "zsh":
		return a.writeZsh(w, t)
	case "bash":
		return a.writeBash(w, t)
	case "fish":
		return a.writeFish(w, t)
	}
	return fmt.Errorf("unsupported shell %q (want one of: %s)", shell, strings.Join(Shells, ", "))
}

// legacyModeFlags returns the px.sh mode flags sorted, for the scripts.
func (a *App) legacyModeFlags() []string {
	var out []string
	for f := range a.Registry.LegacyModes {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// fallbackNode returns the node of the registry's fallback command.
func fallbackNode(root *node, name string) *node {
	for _, s := range root.subs {
		if len(s.path) == 1 && s.path[0] == name {
			return s
		}
	}
	return nil
}
