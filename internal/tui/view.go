package tui

import (
	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// view is one tab. Views are pointers owned by the root model; the root
// routes keys and mouse events to the active view only, and every other
// message (query results, job results) to all views, which ignore what is
// not theirs.
type view interface {
	name() string  // "proxies"
	title() string // "Proxies"
	about() string // one paragraph for the help overlay

	// refresh reloads the view's data (in a tea.Cmd).
	refresh(m *Model) tea.Cmd
	// update handles non-key messages (data, job results, input internals).
	update(m *Model, msg tea.Msg) tea.Cmd
	// key handles a key press while the view is active.
	key(m *Model, msg tea.KeyMsg) tea.Cmd
	// scroll handles the mouse wheel (delta rows); click a left click at
	// x, y relative to the body.
	scroll(m *Model, delta int) tea.Cmd
	click(m *Model, x, y int) tea.Cmd

	// resize lays the view out for a body of w x h and rebuilds styled
	// rows (it also runs after a theme change).
	resize(m *Model, w, h int)
	// render returns the body (the root fits it to w x h).
	render(m *Model) string
	// status returns the status-bar segments (row counts, filters, sort).
	status(m *Model) []string
	// footer replaces the key-hint line while an input is open ("" = hints).
	footer(m *Model) string
	// capturing is true while a text input has the keyboard: global keys
	// (q, 1-7, tab, ...) then go to the view.
	capturing() bool
	// help lists the view's own keys (help overlay); hints the few shown in
	// the bottom line.
	help() []key.Binding
	hints() []hint
}

// baseView provides defaults for the optional parts of view.
type baseView struct {
	w, h int
}

func (b *baseView) scroll(*Model, int) tea.Cmd     { return nil }
func (b *baseView) click(*Model, int, int) tea.Cmd { return nil }
func (b *baseView) footer(*Model) string           { return "" }
func (b *baseView) capturing() bool                { return false }

// newInput is a text input styled with the current theme (static cursor:
// no blink ticks).
func newInput(m *Model, prompt, placeholder string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = prompt
	ti.Placeholder = placeholder
	ti.Cursor.SetMode(cursor.CursorStatic)
	styleInput(m, &ti)
	return ti
}

// styleInput (re)applies the theme to a text input.
func styleInput(m *Model, ti *textinput.Model) {
	s := m.styles
	ti.PromptStyle = s.Prompt
	ti.TextStyle = s.Input
	ti.PlaceholderStyle = s.Placeholder
	ti.CompletionStyle = s.Faint
	ti.Cursor.Style = s.Accent
	ti.Cursor.TextStyle = s.Input
}
