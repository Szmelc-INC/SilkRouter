package tui

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/cli"
	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/scan"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

// command is one ':' palette command.
type command struct {
	name    string
	aliases []string
	args    string // synopsis of the arguments
	help    string
	// complete returns the candidates for argument i (0-based) given the
	// arguments typed before it.
	complete func(m *Model, i int, prev []string) []string
	run      func(m *Model, args []string) tea.Cmd
}

var (
	cmdOnce sync.Once
	cmds    []command
)

// commandList is the palette registry (built lazily: the help command
// renders the registry itself).
func commandList() []command {
	cmdOnce.Do(func() { cmds = buildCommands() })
	return cmds
}

func words(ws ...string) func(*Model, int, []string) []string {
	return func(*Model, int, []string) []string { return ws }
}

func buildCommands() []command {
	return []command{
		{name: "q", aliases: []string{"quit", "exit"}, help: "quit (asks while background jobs run)",
			run: func(m *Model, _ []string) tea.Cmd { return m.requestQuit(false) }},
		{name: "q!", aliases: []string{"quit!"}, help: "quit now, cancelling background jobs",
			run: func(m *Model, _ []string) tea.Cmd { return m.requestQuit(true) }},
		{name: "tab", args: "NAME|1-7", help: "switch tab (dashboard, proxies, history, tags, scans, lists, config)",
			complete: func(*Model, int, []string) []string { return TabNames() },
			run: func(m *Model, args []string) tea.Cmd {
				if len(args) != 1 {
					return m.notify(levelErr, "usage: :tab NAME")
				}
				i, err := tabIndex(args[0])
				if err != nil {
					return m.notify(levelErr, "%v", err)
				}
				return m.switchTab(i)
			}},
		{name: "sort", args: "KEY [asc|desc]", help: "sort the proxy table by any px proxies --sort key",
			complete: func(_ *Model, i int, _ []string) []string {
				if i == 0 {
					return db.SortKeys()
				}
				return []string{"asc", "desc"}
			},
			run: func(m *Model, args []string) tea.Cmd {
				if len(args) < 1 || len(args) > 2 {
					return m.notify(levelErr, "usage: :sort KEY [asc|desc]")
				}
				dir := ""
				if len(args) == 2 {
					dir = args[1]
				}
				f, err := m.prox.f.withSort(args[0], dir)
				if err != nil {
					return m.notify(levelErr, "%v", err)
				}
				m.prox.f = f
				m.toProxies()
				return m.prox.reloadFiltered(m)
			}},
		{name: "filter", args: "[KEY=VALUE...|clear]", help: "filter the proxies: status scheme tag max_ms min_ms country listed_max min_streak source anon https dead search",
			complete: completeFilter,
			run: func(m *Model, args []string) tea.Cmd {
				f, err := m.prox.f.apply(args)
				if err != nil {
					return m.notify(levelErr, "%v", err)
				}
				m.prox.f = f
				m.toProxies()
				return m.prox.reloadFiltered(m)
			}},
		{name: "search", aliases: []string{"find"}, args: "TEXT", help: "search the current tab (proxies, history, tags, config); empty clears",
			run: func(m *Model, args []string) tea.Cmd { return m.search(strings.Join(args, " ")) }},
		{name: "tag", args: "add|rm NAME[:SCOPE]...", help: "tag the selected proxy, or remove tags from it",
			complete: func(m *Model, i int, prev []string) []string {
				if i == 0 {
					return []string{"add", "rm"}
				}
				if len(prev) > 0 && prev[0] == "rm" {
					if p := m.selected(); p != nil {
						return p.Tags
					}
					return nil
				}
				return dedupe(append([]string{"blocked", "pinned"}, m.tagLabels...))
			},
			run: func(m *Model, args []string) tea.Cmd {
				if len(args) < 2 || (args[0] != "add" && args[0] != "rm" && args[0] != "remove") {
					return m.notify(levelErr, "usage: :tag add|rm NAME...")
				}
				p := m.selected()
				if p == nil {
					return m.notify(levelWarn, "select a proxy in the Proxies tab first")
				}
				if args[0] == "add" {
					return m.addTags(p.ID, p.Key(), args[1:])
				}
				return m.removeTags(p.ID, p.Key(), args[1:])
			}},
		{name: "block", help: "add the manual blocked tag to the selected proxy (route skips it)",
			run: func(m *Model, _ []string) tea.Cmd { on := true; return m.setBlocked(m.selected(), &on) }},
		{name: "unblock", help: "remove the blocked tag from the selected proxy",
			run: func(m *Model, _ []string) tea.Cmd { off := false; return m.setBlocked(m.selected(), &off) }},
		{name: "check", help: "check the selected proxy now (background; recorded as a probe measurement)",
			run: func(m *Model, _ []string) tea.Cmd { return m.checkProxy(m.selected()) }},
		{name: "copy", aliases: []string{"yank"}, help: "copy the selected proxy's URL (OSC 52)",
			run: func(m *Model, _ []string) tea.Cmd { return m.copyURL(m.selected()) }},
		{name: "show", aliases: []string{"open"}, args: "ID|URL|HOST[:PORT]", help: "open the details of a proxy",
			run: func(m *Model, args []string) tea.Cmd {
				if len(args) != 1 {
					return m.notify(levelErr, "usage: :show ID")
				}
				d, ctx, ref := m.d, m.ctx, args[0]
				return func() tea.Msg {
					p, err := d.FindProxy(ctx, ref)
					if err != nil {
						return toastMsg{levelErr, err.Error()}
					}
					return showProxyMsg(p.ID)
				}
			}},
		{name: "rank", args: "[MODE]", help: "recompute scores and ranks (balanced, speed, clean, stable), then the tags",
			complete: func(*Model, int, []string) []string { return score.RankModes },
			run: func(m *Model, args []string) tea.Cmd {
				mode := ""
				if len(args) > 0 {
					mode = args[0]
				}
				return m.rank(mode)
			}},
		{name: "scan", args: "[MODE [all|alive|filter|selected] [restart]] | stop",
			help: "start a scan in the background (quick, full, reputation, geo, protocols; default every proxy); :scan alone opens the form, :scan stop pauses",
			complete: func(_ *Model, i int, prev []string) []string {
				if i == 0 {
					return append(append([]string(nil), scan.Modes...), "stop")
				}
				if len(prev) > 0 && prev[0] == "stop" {
					return nil
				}
				return append(append([]string(nil), scanSelections...), "restart")
			},
			run: func(m *Model, args []string) tea.Cmd { return m.paletteScan(args) }},
		{name: "retag", help: "recompute every heuristic tag now (px tags refresh)",
			run: func(m *Model, _ []string) tea.Cmd { return m.retag() }},
		{name: "refresh", aliases: []string{"reload"}, help: "reload the current tab",
			run: func(m *Model, _ []string) tea.Cmd { return m.reloadActive() }},
		{name: "export", args: "FILE", help: "write the URLs of every proxy matching the proxy table's filter to FILE",
			run: func(m *Model, args []string) tea.Cmd { return m.exportURLs(strings.Join(args, " ")) }},
		{name: "theme", args: "NAME", help: "switch the theme for this session (silk, mono, neon; :set tui.theme saves it)",
			complete: func(*Model, int, []string) []string { return ThemeNames() },
			run: func(m *Model, args []string) tea.Cmd {
				if len(args) != 1 {
					return m.notify(levelErr, "usage: :theme NAME (%s)", strings.Join(ThemeNames(), ", "))
				}
				if err := m.setTheme(args[0]); err != nil {
					return m.notify(levelErr, "%v", err)
				}
				return m.notify(levelOK, "theme %s (:set tui.theme %s keeps it)", args[0], args[0])
			}},
		{name: "set", args: "KEY VALUE", help: "write a config key to config.toml (like px config set)",
			complete: func(_ *Model, i int, prev []string) []string {
				if i == 0 {
					return config.KeyNames()
				}
				if k, ok := config.Lookup(prev[0]); ok {
					if len(k.Choices) > 0 {
						return k.Choices
					}
					if k.Type == "bool" {
						return []string{"true", "false"}
					}
				}
				return nil
			},
			run: func(m *Model, args []string) tea.Cmd {
				if len(args) < 2 {
					return m.notify(levelErr, "usage: :set KEY VALUE")
				}
				if err := validate(args[0], strings.Join(args[1:], " ")); err != nil {
					return m.notify(levelErr, "%v", err)
				}
				return m.setConfig(args[0], strings.Join(args[1:], " "), false)
			}},
		{name: "help", aliases: []string{"h", "?"}, help: "show the keys of the current tab and every command",
			run: func(m *Model, _ []string) tea.Cmd { return m.help.show(m) }},
	}
}

type showProxyMsg int64

func completeFilter(m *Model, _ int, _ []string) []string {
	var out []string
	for _, k := range filterKeys {
		out = append(out, k+"=")
		var vals []string
		switch k {
		case "status":
			vals = StatusFilters
		case "scheme":
			vals = schemeCycle
		case "tag":
			vals = m.tagLabels
		case "anon":
			vals = anonValues
		case "https", "dead":
			vals = []string{"true", "false"}
		}
		for _, v := range vals {
			out = append(out, k+"="+v)
		}
	}
	return append(out, "clear")
}

// lookupCommand finds a command by name, alias or unique prefix.
func lookupCommand(name string) (command, error) {
	cs := commandList()
	for _, c := range cs {
		if c.name == name || contains(c.aliases, name) {
			return c, nil
		}
	}
	var found []command
	var names []string
	for _, c := range cs {
		names = append(names, c.name)
		if strings.HasPrefix(c.name, name) {
			found = append(found, c)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	msg := fmt.Sprintf("unknown command :%s", name)
	if len(found) > 1 {
		var ns []string
		for _, c := range found {
			ns = append(ns, ":"+c.name)
		}
		msg = fmt.Sprintf(":%s is ambiguous (%s)", name, strings.Join(ns, ", "))
	} else if s := cli.SuggestFrom(name, names); len(s) > 0 {
		msg += " (did you mean :" + strings.Join(s, " or :") + "?)"
	}
	return command{}, fmt.Errorf("%s; :help lists them", msg)
}

// execute runs one palette line.
func (m *Model) execute(line string) tea.Cmd {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ":"))
	if line == "" {
		return nil
	}
	fields := strings.Fields(line)
	c, err := lookupCommand(fields[0])
	if err != nil {
		return m.notify(levelErr, "%v", err)
	}
	return c.run(m, fields[1:])
}

// selected is the proxy palette actions apply to (Proxies tab only).
func (m *Model) selected() *db.Proxy {
	if m.activeView() != view(m.prox) {
		return nil
	}
	return m.prox.current()
}

// toProxies activates the proxy table (details closed).
func (m *Model) toProxies() {
	m.active = indexOf(TabProxies)
	m.prox.detail = nil
}

// search sets the search of the current tab (the proxy table for tabs
// without one).
func (m *Model) search(text string) tea.Cmd {
	switch m.activeView() {
	case view(m.hist):
		m.hist.query = text
		return m.hist.restart(m)
	case view(m.tagv):
		m.tagv.query = text
		m.tagv.tbl.reset()
		m.tagv.rebuild(m)
		return nil
	case view(m.conf):
		m.conf.query = text
		m.conf.tbl.reset()
		m.conf.rebuild(m)
		return nil
	}
	m.toProxies()
	m.prox.f.search = text
	return m.prox.reloadFiltered(m)
}

// ---- the palette model ------------------------------------------------------------

type palette struct {
	open    bool
	input   textinput.Model
	cands   []string // completion candidates of the token under the cursor
	sel     int      // candidate inserted by the last tab (-1 none)
	base    string   // the line without the token being completed
	history []string
	hpos    int
}

func newPalette(m *Model) *palette {
	p := &palette{sel: -1}
	p.input = newInput(m, ":", "command (tab completes, ↑↓ history, esc closes)")
	return p
}

func (p *palette) resize(m *Model) {
	styleInput(m, &p.input)
	p.input.Width = max(10, m.width-4)
}

func (p *palette) show(m *Model, text string) tea.Cmd {
	p.open = true
	p.input.SetValue(text)
	p.input.CursorEnd()
	p.hpos = len(p.history)
	p.recompute(m)
	return tea.Batch(p.input.Focus(), m.loadTagLabels())
}

func (p *palette) close() {
	p.open = false
	p.input.Blur()
	p.cands = nil
}

// recompute derives the candidates of the token being typed.
func (p *palette) recompute(m *Model) {
	p.sel = -1
	val := p.input.Value()
	fields := strings.Fields(val)
	trailing := strings.HasSuffix(val, " ") || val == ""
	idx := len(fields) - 1
	partial := ""
	if trailing {
		idx = len(fields)
	} else if idx >= 0 {
		partial = fields[idx]
	}
	p.base = strings.TrimSuffix(val, partial)
	var all []string
	if idx <= 0 {
		for _, c := range commandList() {
			all = append(all, c.name)
		}
	} else if c, err := lookupCommand(fields[0]); err == nil && c.complete != nil {
		prev := fields[1:idx]
		all = c.complete(m, idx-1, prev)
	}
	p.cands = nil
	for _, c := range all {
		if strings.HasPrefix(c, partial) && c != partial {
			p.cands = append(p.cands, c)
		}
	}
	if idx > 0 || partial != "" {
		sort.SliceStable(p.cands, func(i, j int) bool { return len(p.cands[i]) < len(p.cands[j]) })
	}
}

func (p *palette) complete(step int) {
	if len(p.cands) == 0 {
		return
	}
	p.sel = ((p.sel+step)%len(p.cands) + len(p.cands)) % len(p.cands)
	val := p.base + p.cands[p.sel]
	if len(p.cands) == 1 && !strings.HasSuffix(val, "=") {
		val += " "
	}
	p.input.SetValue(val)
	p.input.CursorEnd()
}

func (p *palette) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		p.close()
		return nil
	case "enter":
		line := p.input.Value()
		p.close()
		if strings.TrimSpace(line) != "" {
			if n := len(p.history); n == 0 || p.history[n-1] != line {
				p.history = append(p.history, line)
			}
		}
		return m.execute(line)
	case "tab":
		single := len(p.cands) == 1
		p.complete(1)
		if single {
			p.recompute(m)
		}
		return nil
	case "shift+tab":
		p.complete(-1)
		return nil
	case "up":
		if p.hpos > 0 {
			p.hpos--
			p.input.SetValue(p.history[p.hpos])
			p.input.CursorEnd()
			p.recompute(m)
		}
		return nil
	case "down":
		if p.hpos < len(p.history) {
			p.hpos++
			v := ""
			if p.hpos < len(p.history) {
				v = p.history[p.hpos]
			}
			p.input.SetValue(v)
			p.input.CursorEnd()
			p.recompute(m)
		}
		return nil
	case "backspace":
		if p.input.Value() == "" {
			p.close()
			return nil
		}
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	p.recompute(m)
	return cmd
}

func (p *palette) update(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tagLabelsMsg:
		m.tagLabels = []string(msg)
		if p.open {
			p.recompute(m)
		}
	case showProxyMsg:
		return m.showProxy(int64(msg))
	default:
		if p.open {
			var cmd tea.Cmd
			p.input, cmd = p.input.Update(msg)
			return cmd
		}
	}
	return nil
}

// statusLine shows the candidates (or the matched command's synopsis)
// above the input.
func (p *palette) statusLine(m *Model) string {
	s := m.styles
	if len(p.cands) > 0 {
		parts := make([]string, 0, len(p.cands))
		for i, c := range p.cands {
			if i == p.sel {
				if s.plain {
					c = "[" + c + "]"
				} else {
					c = s.Selected.Render(c)
				}
			} else {
				c = s.Accent2.Render(c)
			}
			parts = append(parts, c)
		}
		return " " + strings.Join(parts, "  ")
	}
	fields := strings.Fields(p.input.Value())
	if len(fields) > 0 {
		if c, err := lookupCommand(fields[0]); err == nil {
			return " " + s.Accent2.Render(":"+c.name) + " " + s.Text.Render(c.args) + s.Muted.Render("  "+c.help)
		}
	}
	return " " + s.Muted.Render("commands: ") + s.Faint.Render(strings.Join(func() []string {
		var ns []string
		for _, c := range commandList() {
			ns = append(ns, c.name)
		}
		return ns
	}(), " "))
}
