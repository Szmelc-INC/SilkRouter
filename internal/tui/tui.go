// Package tui is SilkRouter's interactive terminal UI (px tui): a tabbed
// Bubble Tea program over the database.
//
// Layout of the package:
//
//	tui.go          Options, New, Run
//	model.go        the root model: tabs, routing of keys / mouse / messages, layout
//	view.go         the view interface every tab implements
//	view_*.go       one file per tab (dashboard, proxies + detail, history, tags,
//	                scans, lists, config)
//	actions.go      background work shared by keys and the palette (check, tags,
//	                rank, export, config writes, clipboard)
//	palette.go      the ':' command palette and its command registry
//	help.go         the '?' overlay (markdown via glamour, or plain text)
//	table.go        the scrollable table used by every list
//	styles.go       themes (silk, mono, neon) and semantic colors
//	keymap.go       key bindings (help is generated from them)
//	format.go       small formatting helpers
//
// Every database query runs inside a tea.Cmd, so Update never blocks; the
// package does not import package main: the CLI passes the database, the
// configuration and a few callbacks through Options.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/reputation"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

// Tab names, in tab-bar order (keys 1-7).
const (
	TabDashboard = "dashboard"
	TabProxies   = "proxies"
	TabHistory   = "history"
	TabTags      = "tags"
	TabScans     = "scans"
	TabLists     = "lists"
	TabConfig    = "config"
)

var tabOrder = []string{TabDashboard, TabProxies, TabHistory, TabTags, TabScans, TabLists, TabConfig}

// TabNames lists the tabs in tab-bar order.
func TabNames() []string { return append([]string(nil), tabOrder...) }

// StatusFilters are the values of the proxy table's status filter.
var StatusFilters = []string{"all", db.StatusAlive, db.StatusDead, db.StatusUnknown}

// Options is everything the TUI needs from its caller.
type Options struct {
	// DB is the open database (required).
	DB *db.DB
	// Config is the effective configuration (defaults < file < environment).
	Config *config.Config
	// ConfigPath is the config.toml the Config tab (and :set) writes, the
	// way 'px config set' does.
	ConfigPath string
	// Prog is the invoked program name (px, s-proxy, silkrouter).
	Prog    string
	Version string

	// Start state, usually from flags (zero values fall back to the [tui]
	// config keys where one exists).
	Tab      string // a TabNames entry, a unique prefix or 1-7
	Theme    string // ThemeNames
	Search   string // initial proxy search
	Sort     string // db.SortKeys
	Reverse  bool
	Status   string // StatusFilters
	ShowDead bool   // show dead rows while the status filter is "all"
	// Refresh is the auto-refresh interval of the visible tab (0 = manual).
	Refresh   time.Duration
	Mouse     bool
	AltScreen bool
	// Color is "auto", "always" or "never" (ui.Mode()); never selects the
	// Ascii color profile (no escape sequences at all).
	Color string

	// Callbacks shared with the CLI (nil = built-in equivalents).
	ScoreOptions func(cfg *config.Config, mode string) score.Options
	TagOptions   func(cfg *config.Config) score.TagOptions
	ListManager  func(cfg *config.Config) (*reputation.Manager, error)

	// Getenv reads SILKROUTER_ACTIVE / SILKROUTER_PROXY / SILKROUTER_SESSION
	// and TMUX / TERM (default os.Getenv).
	Getenv func(string) string
	// Clipboard receives the OSC 52 sequence of 'y' (default Output).
	Clipboard io.Writer
	// Input and Output of the program (default stdin / stdout).
	Input  io.Reader
	Output io.Writer
}

// tabIndex resolves a tab name, unique prefix or 1-based number.
func tabIndex(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil {
		if n >= 1 && n <= len(tabOrder) {
			return n - 1, nil
		}
		return 0, fmt.Errorf("no tab %d (1-%d)", n, len(tabOrder))
	}
	found := -1
	for i, t := range tabOrder {
		if t == s {
			return i, nil
		}
		if strings.HasPrefix(t, s) && s != "" {
			if found >= 0 {
				return 0, fmt.Errorf("tab %q is ambiguous", s)
			}
			found = i
		}
	}
	if found < 0 {
		return 0, fmt.Errorf("unknown tab %q (want one of: %s)", s, strings.Join(tabOrder, ", "))
	}
	return found, nil
}

// New builds the root model without starting a program (tests drive it
// with Update / View directly).
func New(ctx context.Context, opts Options) (*Model, error) {
	if opts.DB == nil {
		return nil, errors.New("tui: no database")
	}
	if opts.Config == nil {
		opts.Config = config.Default()
	}
	cfg := opts.Config
	if opts.Prog == "" {
		opts.Prog = "px"
	}
	if opts.Tab == "" {
		opts.Tab = cfg.TUI.StartTab
	}
	if opts.Theme == "" {
		opts.Theme = cfg.TUI.Theme
	}
	if opts.Sort == "" {
		opts.Sort = cfg.TUI.DefaultSort
	}
	if opts.Status == "" {
		opts.Status = "all"
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Output == nil {
		opts.Output = os.Stdout
	}
	if opts.Clipboard == nil {
		opts.Clipboard = opts.Output
	}
	active, err := tabIndex(opts.Tab)
	if err != nil {
		return nil, err
	}
	theme, ok := themeByName(opts.Theme)
	if !ok && opts.Theme != "" {
		return nil, fmt.Errorf("unknown theme %q (want one of: %s)", opts.Theme, strings.Join(ThemeNames(), ", "))
	}
	if !contains(db.SortKeys(), opts.Sort) {
		if opts.Sort != cfg.TUI.DefaultSort {
			return nil, fmt.Errorf("unknown sort key %q (want one of: %s)", opts.Sort, strings.Join(db.SortKeys(), ", "))
		}
		opts.Sort = "rank" // a bad tui.default_sort in the file
	}
	if !contains(StatusFilters, opts.Status) {
		return nil, fmt.Errorf("unknown status %q (want one of: %s)", opts.Status, strings.Join(StatusFilters, ", "))
	}

	r := lipgloss.NewRenderer(opts.Output)
	switch opts.Color {
	case "never":
		r.SetColorProfile(termenv.Ascii)
	case "always":
		if r.ColorProfile() == termenv.Ascii {
			r.SetColorProfile(termenv.ANSI256)
		}
	}
	// No adaptive colors are used, so lipgloss never queries the terminal
	// background (which can stall or leak a reply into the input).
	r.SetHasDarkBackground(true)

	mctx, cancel := context.WithCancel(ctx)
	m := &Model{
		ctx:          mctx,
		cancel:       cancel,
		opts:         opts,
		d:            opts.DB,
		cfg:          cfg,
		renderer:     r,
		theme:        theme.Name,
		styles:       newStyles(r, theme),
		active:       active,
		jobs:         map[int]string{},
		refreshEvery: opts.Refresh,
		tick:         tea.Tick,
		width:        100,
		height:       30,
	}
	m.dash = newDashboardView()
	m.prox = newProxiesView(m, opts)
	m.hist = newHistoryView(m)
	m.tagv = newTagsView(m)
	m.scans = newScansView()
	m.lists = newListsView()
	m.conf = newConfigView(m)
	m.views = []view{m.dash, m.prox, m.hist, m.tagv, m.scans, m.lists, m.conf}
	m.pal = newPalette(m)
	m.help = &helpOverlay{}
	m.layout()
	return m, nil
}

// Run starts the program on the terminal and returns when the user quits
// (or ctx is cancelled). Background work is cancelled on return.
func Run(ctx context.Context, opts Options) error {
	m, err := New(ctx, opts)
	if err != nil {
		return err
	}
	defer m.cancel()
	// Components that build their own styles (bubbles) use the default
	// renderer: make it ours, so --no-color holds everywhere.
	lipgloss.SetDefaultRenderer(m.renderer)
	popts := []tea.ProgramOption{tea.WithContext(ctx)}
	if opts.AltScreen {
		popts = append(popts, tea.WithAltScreen())
	}
	if opts.Mouse {
		popts = append(popts, tea.WithMouseCellMotion())
	}
	if opts.Input != nil {
		popts = append(popts, tea.WithInput(opts.Input))
	}
	if opts.Output != nil {
		popts = append(popts, tea.WithOutput(opts.Output))
	}
	_, err = tea.NewProgram(m, popts...).Run()
	switch {
	case errors.Is(err, tea.ErrInterrupted):
		return nil // SIGINT from outside the terminal
	case errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil:
		return nil // the CLI context was cancelled (SIGINT / SIGTERM)
	}
	return err
}
