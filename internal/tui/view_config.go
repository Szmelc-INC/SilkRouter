package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/paths"
)

type configLoadedMsg struct {
	cfg *config.Config
	err error
}

// configView lists every configuration key and edits values in place,
// writing config.toml the way 'px config set' does.
type configView struct {
	baseView
	tbl  table
	keys []config.Key // shown (filtered) keys

	query      string
	search     textinput.Model
	searching  bool
	prevSearch string

	edit    textinput.Model
	editing bool
	editKey string
	editErr string
	saving  bool
}

func newConfigView(m *Model) *configView {
	v := &configView{}
	v.search = newInput(m, "/", "key or help text")
	v.edit = newInput(m, "", "")
	v.tbl.setColumns([]column{{title: "KEY", max: 28}, {title: "VALUE", max: 30, flex: true, min: 8}, {title: "ORIGIN", max: 30},
		{title: "DEFAULT", max: 22, flex: true, min: 6}, {title: "HELP", flex: true, min: 10}})
	v.tbl.empty = "no key matches"
	return v
}

func (v *configView) name() string  { return TabConfig }
func (v *configView) title() string { return "Config" }
func (v *configView) about() string {
	return "Every configuration key with its effective value, where the value comes from (default, file, or an " +
		"environment variable) and its default. Enter edits a value in place: it is validated as you type, then " +
		"written to config.toml exactly like `px config set` (canonical commented file, previous one kept as " +
		"config.toml.bak); `x` resets a key to its default. [tui] keys apply immediately."
}

func (v *configView) capturing() bool { return v.searching || v.editing }

func showValue(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

func (v *configView) rebuild(m *Model) {
	s := m.styles
	v.keys = v.keys[:0]
	q := strings.ToLower(v.query)
	for _, k := range config.Keys() {
		if q == "" || strings.Contains(k.Name, q) || strings.Contains(strings.ToLower(k.Help), q) {
			v.keys = append(v.keys, k)
		}
	}
	rows := make([][]cell, len(v.keys))
	for i, k := range v.keys {
		val, _ := m.cfg.Get(k.Name)
		vc := cell{showValue(val), s.Text}
		if val != k.Default {
			vc.style = s.Good
		}
		origin := m.cfg.Origin(k.Name)
		oc := cell{origin, s.Muted}
		switch {
		case origin == config.FromFile:
			oc.style = s.Accent
		case strings.HasPrefix(origin, "env "):
			oc.style = s.Warn
		}
		rows[i] = []cell{{k.Name, s.Accent2}, vc, oc, {showValue(k.Default), s.Muted}, {k.Help, s.Muted}}
	}
	v.tbl.setRows(rows)
}

func (v *configView) resize(m *Model, w, h int) {
	v.w, v.h = w, h
	styleInput(m, &v.search)
	styleInput(m, &v.edit)
	v.search.Width = max(10, w-4)
	v.edit.Width = max(10, w/2)
	v.tbl.setHeight(h - 5)
	v.rebuild(m)
}

// refresh re-reads the configuration (defaults < file < environment), so
// edits made elsewhere show up.
func (v *configView) refresh(m *Model) tea.Cmd {
	path := m.opts.ConfigPath
	if path == "" {
		v.rebuild(m)
		return nil
	}
	return func() tea.Msg {
		c, err := config.Load(path, os.LookupEnv)
		return configLoadedMsg{cfg: c, err: err}
	}
}

func (v *configView) current() (config.Key, bool) {
	i := v.tbl.selected()
	if i < 0 || i >= len(v.keys) {
		return config.Key{}, false
	}
	return v.keys[i], true
}

func (v *configView) update(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case configLoadedMsg:
		if msg.cfg != nil && msg.err == nil {
			m.cfg = msg.cfg
		}
		v.rebuild(m)
		if msg.err != nil {
			return m.notify(levelErr, "%v", msg.err)
		}
	case configSavedMsg:
		if v.saving && msg.key == v.editKey {
			v.saving = false
			if msg.err != nil {
				v.editErr = msg.err.Error()
			} else {
				v.editing = false
				v.editErr = ""
				v.edit.Blur()
			}
		}
		v.rebuild(m)
	default:
		var cmd tea.Cmd
		switch {
		case v.editing:
			v.edit, cmd = v.edit.Update(msg)
		case v.searching:
			v.search, cmd = v.search.Update(msg)
		}
		return cmd
	}
	return nil
}

// validate checks a value against the key's type, choices and minimum
// without touching anything.
func validate(k, value string) error {
	return config.Default().Set(k, value)
}

func (v *configView) startEdit(m *Model) tea.Cmd {
	k, ok := v.current()
	if !ok {
		return nil
	}
	val, _ := m.cfg.Get(k.Name)
	v.editing, v.editKey, v.editErr, v.saving = true, k.Name, "", false
	v.edit.Prompt = " " + k.Name + " (" + k.Type + ") › "
	v.edit.Placeholder = "default " + showValue(k.Default)
	v.edit.SetValue(val)
	v.edit.CursorEnd()
	switch {
	case len(k.Choices) > 0:
		v.edit.SetSuggestions(k.Choices)
		v.edit.ShowSuggestions = true
	case k.Type == "bool":
		v.edit.SetSuggestions([]string{"true", "false"})
		v.edit.ShowSuggestions = true
	default:
		v.edit.SetSuggestions(nil)
		v.edit.ShowSuggestions = false
	}
	return v.edit.Focus()
}

func (v *configView) key(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch {
	case v.editing:
		switch msg.String() {
		case "esc":
			v.editing, v.saving, v.editErr = false, false, ""
			v.edit.Blur()
			return nil
		case "enter":
			if v.saving {
				return nil
			}
			val := v.edit.Value()
			if err := validate(v.editKey, val); err != nil {
				v.editErr = err.Error()
				return nil
			}
			v.saving = true
			return m.setConfig(v.editKey, val, false)
		}
		var cmd tea.Cmd
		v.edit, cmd = v.edit.Update(msg)
		v.editErr = ""
		if err := validate(v.editKey, v.edit.Value()); err != nil {
			v.editErr = err.Error()
		}
		return cmd
	case v.searching:
		switch msg.String() {
		case "enter":
			v.searching = false
			v.search.Blur()
			return nil
		case "esc":
			v.searching = false
			v.search.Blur()
			v.query = v.prevSearch
			v.rebuild(m)
			return nil
		}
		var cmd tea.Cmd
		v.search, cmd = v.search.Update(msg)
		v.query = v.search.Value()
		v.tbl.reset()
		v.rebuild(m)
		return cmd
	}
	if navigate(&v.tbl, msg, keys.Up) {
		return nil
	}
	switch {
	case key.Matches(msg, keys.Edit):
		return v.startEdit(m)
	case key.Matches(msg, keys.ResetKey):
		if k, ok := v.current(); ok {
			return m.setConfig(k.Name, "", true)
		}
	case key.Matches(msg, keys.Search):
		v.searching = true
		v.prevSearch = v.query
		v.search.SetValue(v.query)
		v.search.CursorEnd()
		return v.search.Focus()
	case key.Matches(msg, keys.Clear):
		v.query = ""
		v.rebuild(m)
	}
	return nil
}

func (v *configView) scroll(m *Model, d int) tea.Cmd {
	if !v.editing {
		v.tbl.move(d)
	}
	return nil
}

func (v *configView) click(m *Model, _, y int) tea.Cmd {
	if v.capturing() {
		return nil
	}
	i := v.tbl.rowAt(y)
	if i < 0 {
		return nil
	}
	if i == v.tbl.cursor {
		return v.startEdit(m)
	}
	v.tbl.cursor = i
	v.tbl.clamp()
	return nil
}

func (v *configView) render(m *Model) string {
	s := m.styles
	body := fitLines(v.tbl.view(s, v.w), v.w, max(1, v.h-5))
	var info []string
	if k, ok := v.current(); ok {
		head := s.Accent2.Render(k.Name) + "  " + s.Muted.Render(k.Type)
		if len(k.Choices) > 0 {
			head += s.Muted.Render(" · one of: ") + strings.Join(k.Choices, ", ")
		}
		if k.Min != nil {
			head += s.Muted.Render(fmt.Sprintf(" · min %g", *k.Min))
		}
		info = append(info, head, s.Text.Render(k.Help))
		env := "env: " + strings.Join(k.Env, ", ")
		if src := m.cfg.Origin(k.Name); strings.HasPrefix(src, "env ") {
			env += s.Warn.Render("  (the environment overrides the file: $" + strings.TrimPrefix(src, "env ") + ")")
		}
		info = append(info, s.Muted.Render(env))
	}
	path := m.opts.ConfigPath
	if path == "" {
		path = "(none)"
	}
	info = append(info, s.Faint.Render("file: "+paths.Pretty(path)+" · enter edit · x reset to default · / filter"))
	for len(info) < 4 {
		info = append(info, "")
	}
	return body + "\n" + s.Faint.Render(" "+strings.Repeat("─", max(1, v.w-2))) + "\n " + strings.Join(info, "\n ")
}

func (v *configView) status(m *Model) []string {
	segs := []string{fmt.Sprintf("%d keys", len(v.keys)), "row " + v.tbl.position()}
	if v.query != "" {
		segs = append(segs, "/"+v.query)
	}
	if v.saving {
		segs = append(segs, "saving…")
	}
	return segs
}

func (v *configView) footer(m *Model) string {
	s := m.styles
	switch {
	case v.editing:
		line := v.edit.View()
		switch {
		case v.saving:
			line += s.Muted.Render("   saving…")
		case v.editErr != "":
			line += "   " + s.Error.Render("✗ "+v.editErr)
		default:
			line += "   " + s.Good.Render("✓") + s.Faint.Render(" enter save · esc cancel · tab completes")
		}
		return line
	case v.searching:
		return v.search.View()
	}
	return ""
}

func (v *configView) help() []key.Binding {
	return []key.Binding{keys.Edit, keys.ResetKey, bind("/", "filter keys (name or help)", "/"), bind("X", "clear the filter", "X"),
		bind("tab", "while editing: accept the suggested choice", "tab")}
}

func (v *configView) hints() []hint {
	return []hint{{"↑↓", "move"}, {"enter", "edit"}, {"x", "reset"}, {"/", "filter"}}
}
