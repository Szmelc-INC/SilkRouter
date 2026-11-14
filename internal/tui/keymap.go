package tui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// keyMap holds every key binding. Bindings carry their help text, so the
// help overlay and the hint line are generated from the same source as the
// key handling.
type keyMap struct {
	// global
	Quit, Help, Palette, NextTab, PrevTab, Refresh, Tabs key.Binding
	// navigation (every table)
	Up, Down, PageUp, PageDown, HalfUp, HalfDown, Top, Bottom, PrevPage, NextPage, Enter, Back key.Binding
	Search, Clear                                                                              key.Binding
	// proxies
	Sort, SortBack, Reverse, Status, Scheme, Dead, Tag, Untag, Block, Check, Copy key.Binding
	// detail
	Reload key.Binding
	// history
	Stats, StatsSort, Result, Mode key.Binding
	// tags / scans / lists / config
	Retag, Rank, Update, ForceUpdate, Edit, ResetKey key.Binding
	// scans: up without k (k ranks)
	UpNoK key.Binding
}

func bind(help, desc string, ks ...string) key.Binding {
	return key.NewBinding(key.WithKeys(ks...), key.WithHelp(help, desc))
}

var keys = keyMap{
	Quit:    bind("q", "quit (ctrl+c too)", "q", "ctrl+c"),
	Help:    bind("?", "toggle this help", "?"),
	Palette: bind(":", "command palette", ":"),
	NextTab: bind("tab", "next tab", "tab"),
	PrevTab: bind("shift+tab", "previous tab", "shift+tab"),
	Refresh: bind("ctrl+r", "reload the view (F5 too)", "ctrl+r", "f5"),
	Tabs:    bind("1-7", "jump to a tab", "1", "2", "3", "4", "5", "6", "7"),

	Up:       bind("↑/k", "up", "up", "k"),
	Down:     bind("↓/j", "down", "down", "j"),
	PageUp:   bind("pgup", "page up (ctrl+b)", "pgup", "ctrl+b"),
	PageDown: bind("pgdn", "page down (ctrl+f)", "pgdown", "ctrl+f"),
	HalfUp:   bind("ctrl+u", "half page up", "ctrl+u"),
	HalfDown: bind("ctrl+d", "half page down", "ctrl+d"),
	Top:      bind("home", "first row", "home"),
	Bottom:   bind("end/G", "last row", "end", "G"),
	PrevPage: bind("[", "previous page of rows (tui.page_size)", "["),
	NextPage: bind("]", "next page of rows", "]"),
	Enter:    bind("enter", "open", "enter"),
	Back:     bind("esc", "back", "esc", "backspace", "left", "h"),
	Search:   bind("/", "live search", "/"),
	Clear:    bind("X", "clear filters and search", "X"),

	Sort:     bind("s", "next sort key", "s"),
	SortBack: bind("S", "previous sort key", "S"),
	Reverse:  bind("r", "reverse the sort", "r"),
	Status:   bind("a", "status filter: all / alive / dead / unknown", "a"),
	Scheme:   bind("p", "scheme filter: all / http / https / socks...", "p"),
	Dead:     bind("D", "show / hide dead rows (status all)", "D"),
	Tag:      bind("t", "add tag(s) to the proxy", "t"),
	Untag:    bind("T", "remove tag(s) from the proxy", "T"),
	Block:    bind("b", "toggle the manual blocked tag", "b"),
	Check:    bind("c", "check the proxy now (background)", "c"),
	Copy:     bind("y", "copy the proxy URL (OSC 52)", "y"),

	Reload: bind("r", "reload", "r"),

	Stats:     bind("g", "stats: by proxy, cmd, err, day, ... (cycles)", "g"),
	StatsSort: bind("s", "stats order: uses / last / fail / rate / key", "s"),
	Result:    bind("f", "result filter: all / ok / fail / proxy errors", "f"),
	Mode:      bind("m", "mode filter: all / route / shell / relay", "m"),

	Retag:       bind("R", "refresh the heuristic tags now", "R"),
	Rank:        bind("k", "recompute scores and ranks (+ tags)", "k"),
	Update:      bind("u", "download stale lists (network)", "u"),
	ForceUpdate: bind("U", "download every list again", "U"),
	Edit:        bind("enter/e", "edit the value (writes config.toml)", "enter", "e"),
	ResetKey:    bind("x", "reset the key to its default", "x"),

	UpNoK: bind("↑", "up", "up"),
}

var globalKeys = []key.Binding{keys.Tabs, keys.NextTab, keys.PrevTab, keys.Palette, keys.Help, keys.Refresh, keys.Quit}

var navKeys = []key.Binding{keys.Up, keys.Down, keys.PageUp, keys.PageDown, keys.HalfUp, keys.HalfDown, keys.Top, keys.Bottom}

// navigate applies the common table movement keys and reports whether k
// was one of them. up is keys.Up, or keys.UpNoK where k means something
// else.
func navigate(t *table, k tea.KeyMsg, up key.Binding) bool {
	switch {
	case key.Matches(k, up):
		t.move(-1)
	case key.Matches(k, keys.Down):
		t.move(1)
	case key.Matches(k, keys.PageUp):
		t.page(-1)
	case key.Matches(k, keys.PageDown):
		t.page(1)
	case key.Matches(k, keys.HalfUp):
		t.half(-1)
	case key.Matches(k, keys.HalfDown):
		t.half(1)
	case key.Matches(k, keys.Top):
		t.top()
	case key.Matches(k, keys.Bottom):
		t.bottom()
	default:
		return false
	}
	return true
}
