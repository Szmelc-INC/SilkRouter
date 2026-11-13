package cli

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/ui"
)

// helpWidth is the line width help text wraps at.
func helpWidth() int {
	w := ui.Width(100)
	if w > 120 {
		w = 120
	}
	return w
}

type helpWriter struct {
	w     io.Writer
	c     ui.Palette
	width int
}

func newHelpWriter(w io.Writer) *helpWriter {
	return &helpWriter{w: w, c: ui.ColorsFor(w), width: helpWidth()}
}

func (h *helpWriter) printf(format string, a ...any) { fmt.Fprintf(h.w, format, a...) }

func (h *helpWriter) section(title string) {
	h.printf("\n%s%s%s%s\n", h.c.Bold, h.c.Yellow, strings.ToUpper(title), h.c.Reset)
}

// para prints text wrapped at the help width with the given indent;
// blank lines separate paragraphs and lines starting with whitespace are
// kept verbatim (for small code samples).
func (h *helpWriter) para(text string, indent int) {
	pad := strings.Repeat(" ", indent)
	for i, p := range splitParagraphs(text) {
		if i > 0 {
			h.printf("\n")
		}
		if strings.HasPrefix(p, " ") || strings.HasPrefix(p, "\t") {
			for _, l := range strings.Split(p, "\n") {
				h.printf("%s%s\n", pad, l)
			}
			continue
		}
		for _, l := range wrap(p, h.width-indent) {
			h.printf("%s%s\n", pad, l)
		}
	}
}

func splitParagraphs(text string) []string {
	text = strings.Trim(text, "\n")
	if text == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(text, "\n\n") {
		p = strings.Trim(p, "\n")
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, " ") && !strings.HasPrefix(p, "\t") {
			p = strings.Join(strings.Fields(p), " ")
		}
		out = append(out, p)
	}
	return out
}

// wrap breaks s into lines of at most width runes (words longer than width
// get their own line).
func wrap(s string, width int) []string {
	if width < 20 {
		width = 20
	}
	var lines []string
	var cur strings.Builder
	n := 0
	for _, word := range strings.Fields(s) {
		wl := utf8.RuneCountInString(word)
		if n > 0 && n+1+wl > width {
			lines = append(lines, cur.String())
			cur.Reset()
			n = 0
		}
		if n > 0 {
			cur.WriteByte(' ')
			n++
		}
		cur.WriteString(word)
		n += wl
	}
	if n > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

// row prints an aligned two-column row: left (already coloured, with
// visible width leftWidth) padded to col, then right wrapped with a hanging
// indent.
func (h *helpWriter) row(left string, leftWidth, col int, right string) {
	const indent = 2
	pad := col - leftWidth
	rightWidth := h.width - indent - col - 2
	lines := wrap(right, rightWidth)
	if len(lines) == 0 {
		h.printf("%s%s\n", strings.Repeat(" ", indent), left)
		return
	}
	if pad < 2 {
		// Left column too wide: put the text on the next line.
		h.printf("%s%s\n", strings.Repeat(" ", indent), left)
		pad = col + 2
		for _, l := range lines {
			h.printf("%s%s\n", strings.Repeat(" ", indent+pad), l)
		}
		return
	}
	h.printf("%s%s%s%s\n", strings.Repeat(" ", indent), left, strings.Repeat(" ", pad), lines[0])
	for _, l := range lines[1:] {
		h.printf("%s%s\n", strings.Repeat(" ", indent+col), l)
	}
}

func (h *helpWriter) banner(a *App, prog string) {
	h.printf("%s%s%s%s %s%s%s", h.c.Bold, h.c.Magenta, a.Name, h.c.Reset, h.c.Gray, a.Version, h.c.Reset)
	if a.Tagline != "" {
		h.printf(" %s-%s %s", h.c.Gray, h.c.Reset, a.Tagline)
	}
	h.printf("\n")
}

// WriteHelp renders the overview: banner, usage, grouped commands, global
// options, examples.
func (a *App) WriteHelp(w io.Writer, prog string, cfg *config.Config) {
	h := newHelpWriter(w)
	c := h.c
	h.banner(a, prog)

	h.section("Usage")
	usage := a.Usage
	if len(usage) == 0 {
		usage = []Example{{Cmd: "[global options] <command> [options] [args]"}}
	}
	col := 0
	for _, u := range usage {
		col = max(col, utf8.RuneCountInString(prog+" "+u.Cmd))
	}
	col = min(col, 56)
	for _, u := range usage {
		plain := prog + " " + u.Cmd
		left := c.Bold + prog + c.Reset + " " + u.Cmd
		h.row(left, utf8.RuneCountInString(plain), col+2, dim(c, u.Desc))
	}

	groups := a.Registry.Groups()
	col = 0
	for _, g := range groups {
		for _, cmd := range g.Commands {
			col = max(col, utf8.RuneCountInString(strings.Join(cmd.Names(), ", ")))
		}
	}
	col = min(col+2, 26)
	for _, g := range groups {
		h.section(g.Name)
		for _, cmd := range g.Commands {
			plain := strings.Join(cmd.Names(), ", ")
			left := c.Cyan + c.Bold + cmd.Name + c.Reset
			if len(cmd.Aliases) > 0 {
				left += c.Gray + ", " + strings.Join(cmd.Aliases, ", ") + c.Reset
			}
			h.row(left, utf8.RuneCountInString(plain), col, cmd.Summary)
		}
	}

	gfs := NewFlagSet("global", cfg)
	GlobalFlags(gfs, &Globals{}, false)
	h.section("Global options")
	h.flagTable(gfs.Visible(), cfg)

	if len(a.Examples) > 0 {
		h.section("Examples")
		h.examples(prog, a.Examples)
	}

	h.printf("\n")
	if len(a.Aliases) > 0 {
		h.para(fmt.Sprintf("%s, %s are the same program. Run '%s help <command>' or '%s <command> --help' for details.",
			strings.Join(a.Aliases, ", "), a.Binary, prog, prog), 0)
	}
	if cfg != nil {
		h.printf("%sConfig:%s %s\n", c.Gray, c.Reset, paths.Pretty(cfgPathOf(cfg)))
	}
}

func cfgPathOf(cfg *config.Config) string {
	if p := cfg.Path(); p != "" {
		return p
	}
	return paths.Config()
}

func dim(c ui.Palette, s string) string {
	if s == "" {
		return ""
	}
	return c.Gray + s + c.Reset
}

func (h *helpWriter) examples(prog string, ex []Example) {
	c := h.c
	for i, e := range ex {
		if i > 0 && e.Desc != "" {
			h.printf("\n")
		}
		if e.Desc != "" {
			for _, l := range wrap(e.Desc, h.width-4) {
				h.printf("  %s# %s%s\n", c.Gray, l, c.Reset)
			}
		}
		h.printf("  %s%s%s %s\n", c.Green, prog, c.Reset, e.Cmd)
	}
}

// flagLeft renders the left column of a flag row: "-d, --max-ms MS".
func flagLeft(f *Flag, c ui.Palette) (colored string, width int) {
	var plain, col []string
	for _, n := range f.Short() {
		plain = append(plain, "-"+n)
		col = append(col, c.Cyan+"-"+n+c.Reset)
	}
	for _, n := range f.Long() {
		plain = append(plain, "--"+n)
		col = append(col, c.Cyan+"--"+n+c.Reset)
	}
	p := strings.Join(plain, ", ")
	s := strings.Join(col, ", ")
	if len(f.Short()) == 0 {
		// Align long-only flags with the long column of "-x, --long".
		p = "    " + p
		s = "    " + s
	}
	if f.TakesValue() && f.Meta != "" {
		p += " " + f.Meta
		s += " " + c.Yellow + f.Meta + c.Reset
	}
	return s, utf8.RuneCountInString(p)
}

func (h *helpWriter) flagTable(flags []*Flag, cfg *config.Config) {
	c := h.c
	col := 0
	for _, f := range flags {
		_, w := flagLeft(f, c)
		col = max(col, w)
	}
	col = min(col+2, 34)
	for _, f := range flags {
		left, w := flagLeft(f, c)
		right := f.Usage
		var notes []string
		if f.Default != "" && f.TakesValue() {
			def := f.Default
			if strings.ContainsAny(def, `/\`) {
				def = paths.Pretty(def)
			}
			notes = append(notes, "default: "+def)
		} else if f.IsBool && f.Default == "true" {
			notes = append(notes, "default: on")
		}
		if f.Key != "" {
			src := ""
			if cfg != nil {
				if s := cfg.Origin(f.Key); s != config.FromDefault {
					src = ", from " + s
				}
			}
			notes = append(notes, "config: "+f.Key+src)
		}
		if f.Complete.Kind == CompleteChoices && len(f.Complete.Words) > 0 {
			notes = append(notes, "one of: "+strings.Join(f.Complete.Words, ", "))
		}
		if len(notes) > 0 {
			right += " (" + strings.Join(notes, "; ") + ")"
		}
		h.row(left, w, col, right)
	}
}

// WriteCommandHelp renders the help of the command at path.
func (a *App) WriteCommandHelp(w io.Writer, prog string, path []string, cfg *config.Config) {
	cmd := a.lookupPath(path)
	if cmd == nil {
		a.WriteHelp(w, prog, cfg)
		return
	}
	h := newHelpWriter(w)
	c := h.c
	full := strings.Join(append([]string{prog}, path...), " ")

	h.printf("%s%s%s", c.Bold, full, c.Reset)
	if cmd.Summary != "" {
		h.printf(" %s-%s %s", c.Gray, c.Reset, cmd.Summary)
	}
	h.printf("\n")

	h.section("Usage")
	usage := cmd.Usage
	if usage == "" {
		switch {
		case len(cmd.Subcommands) > 0:
			usage = "<command> [options]"
		default:
			usage = "[options]"
		}
	}
	for _, u := range strings.Split(usage, "\n") {
		h.printf("  %s%s%s %s\n", c.Bold, full, c.Reset, u)
	}
	if len(cmd.Aliases) > 0 {
		h.printf("  %saliases: %s%s\n", c.Gray, strings.Join(cmd.Aliases, ", "), c.Reset)
	}

	if cmd.Description != "" {
		h.section("Description")
		h.para(cmd.Description, 2)
	}

	if subs := cmd.visibleSubs(); len(subs) > 0 {
		h.section("Commands")
		col := 0
		for _, s := range subs {
			col = max(col, utf8.RuneCountInString(strings.Join(s.Names(), ", ")))
		}
		col = min(col+2, 26)
		for _, s := range subs {
			plain := strings.Join(s.Names(), ", ")
			left := c.Cyan + c.Bold + s.Name + c.Reset
			if len(s.Aliases) > 0 {
				left += c.Gray + ", " + strings.Join(s.Aliases, ", ") + c.Reset
			}
			h.row(left, utf8.RuneCountInString(plain), col, s.Summary)
		}
	}

	fs := NewFlagSet(strings.Join(path, " "), cfg)
	if cmd.Flags != nil {
		cmd.Flags(fs)
	}
	var own, global []*Flag
	GlobalFlags(fs, &Globals{}, true)
	for _, f := range fs.Visible() {
		if f.Global {
			global = append(global, f)
		} else {
			own = append(own, f)
		}
	}
	help := &Flag{Names: []string{"h", "help"}, Usage: "show this help", IsBool: true}
	global = append(global, help)
	if len(own) > 0 {
		h.section("Options")
		h.flagTable(own, cfg)
	}
	h.section("Global options")
	h.flagTable(global, cfg)
	h.printf("  %sMore global options (--config, --home, --version): %s --help%s\n", c.Gray, prog, c.Reset)

	if len(cmd.Examples) > 0 {
		h.section("Examples")
		h.examples(prog, cmd.Examples)
	}
	if len(cmd.Subcommands) > 0 {
		h.printf("\n%sRun '%s <command> --help' for details on a command.%s\n", c.Gray, full, c.Reset)
	}
}

// lookupPath finds the command at the canonical (or alias) path.
func (a *App) lookupPath(path []string) *Command {
	if len(path) == 0 {
		return nil
	}
	cmd := a.Registry.Lookup(path[0])
	for _, p := range path[1:] {
		if cmd == nil {
			return nil
		}
		cmd = cmd.Sub(p)
	}
	return cmd
}

// ResolveHelpPath maps user-typed names ("ls", "config set") to the
// canonical path, or returns an error for unknown names.
func (a *App) ResolveHelpPath(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	cmd := a.Registry.Lookup(names[0])
	if cmd == nil {
		return nil, &UnknownCommandError{Name: names[0], Suggest: a.Registry.Suggest(names[0])}
	}
	path := []string{cmd.Name}
	for _, n := range names[1:] {
		sub := cmd.Sub(n)
		if sub == nil {
			return nil, &UnknownCommandError{Parent: strings.Join(path, " "), Name: n, Suggest: suggest(n, subNames(cmd))}
		}
		cmd = sub
		path = append(path, sub.Name)
	}
	return path, nil
}
