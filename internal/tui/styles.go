package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

// Theme is a named color set. Colors are hex (true color, degraded by
// lipgloss on 256/16-color terminals) or ANSI-256 indexes.
type Theme struct {
	Name    string
	Accent  lipgloss.Color // active tab, titles, mode pill
	Accent2 lipgloss.Color // proxy URLs, keys
	Text    lipgloss.Color
	Muted   lipgloss.Color
	Faint   lipgloss.Color
	Good    lipgloss.Color
	Warn    lipgloss.Color
	Bad     lipgloss.Color
	Info    lipgloss.Color
	Tag     lipgloss.Color
	SelFg   lipgloss.Color // selected row
	SelBg   lipgloss.Color
	BarFg   lipgloss.Color // status bar
	BarBg   lipgloss.Color
	// Glamour is the glamour standard style used for markdown help.
	Glamour string
}

var themeList = []Theme{
	{
		Name: "silk", Accent: "#C3A6FF", Accent2: "#F5A9D0", Text: "#E8E3F5", Muted: "#9890B0", Faint: "#5E5873",
		Good: "#9DE0AD", Warn: "#F7D58B", Bad: "#F28FAD", Info: "#8FD3F0", Tag: "#F0B8E8",
		SelFg: "#FFFFFF", SelBg: "#4A3B6B", BarFg: "#D9D2EE", BarBg: "#2B2440", Glamour: "dracula",
	},
	{
		Name: "mono", Accent: "255", Accent2: "252", Text: "252", Muted: "246", Faint: "240",
		Good: "255", Warn: "250", Bad: "243", Info: "250", Tag: "250",
		SelFg: "16", SelBg: "252", BarFg: "252", BarBg: "237", Glamour: "notty",
	},
	{
		Name: "neon", Accent: "#00F0FF", Accent2: "#FF2BD6", Text: "#E6FFFF", Muted: "#8A8FC7", Faint: "#4A4D7A",
		Good: "#39FF14", Warn: "#FFE81F", Bad: "#FF3864", Info: "#2DE2E6", Tag: "#FF6EC7",
		SelFg: "#0B0221", SelBg: "#00F0FF", BarFg: "#00F0FF", BarBg: "#1A0B3B", Glamour: "tokyo-night",
	},
}

// ThemeNames lists the built-in themes (tui.theme choices).
func ThemeNames() []string {
	out := make([]string, len(themeList))
	for i, t := range themeList {
		out[i] = t.Name
	}
	return out
}

func themeByName(name string) (Theme, bool) {
	for _, t := range themeList {
		if t.Name == name {
			return t, true
		}
	}
	return themeList[0], false
}

// Styles are the lipgloss styles of one theme, built on the program's
// renderer (so --no-color / NO_COLOR yield plain text everywhere).
type Styles struct {
	theme Theme
	r     *lipgloss.Renderer
	// plain is set for the Ascii color profile: no colors or attributes at
	// all, so selection and the active tab use text markers instead.
	plain bool

	Text, Muted, Faint, Bold, Good, Warn, Bad, Info, Tag, Accent, Accent2, Title lipgloss.Style

	Header, Selected, Marker lipgloss.Style

	Brand, TabActive, TabInactive, TabBar lipgloss.Style

	ModePill, StatusBar, Hint, HintKey      lipgloss.Style
	ToastInfo, ToastOK, ToastWarn, ToastErr lipgloss.Style

	Card, CardTitle, Overlay          lipgloss.Style
	Prompt, Input, Placeholder, Error lipgloss.Style
	PillGood, PillBad, PillMuted      lipgloss.Style
}

func newStyles(r *lipgloss.Renderer, t Theme) *Styles {
	s := &Styles{theme: t, r: r, plain: r.ColorProfile() == termenv.Ascii}
	ns := r.NewStyle
	s.Text = ns().Foreground(t.Text)
	s.Muted = ns().Foreground(t.Muted)
	s.Faint = ns().Foreground(t.Faint)
	s.Bold = ns().Foreground(t.Text).Bold(true)
	s.Good = ns().Foreground(t.Good)
	s.Warn = ns().Foreground(t.Warn)
	s.Bad = ns().Foreground(t.Bad)
	s.Info = ns().Foreground(t.Info)
	s.Tag = ns().Foreground(t.Tag)
	s.Accent = ns().Foreground(t.Accent)
	s.Accent2 = ns().Foreground(t.Accent2)
	s.Title = ns().Foreground(t.Accent).Bold(true)
	if t.Name == "mono" {
		s.Good = s.Good.Bold(true)
		s.Bad = s.Bad.Faint(true)
	}

	s.Header = ns().Foreground(t.Muted).Bold(true)
	s.Selected = ns().Foreground(t.SelFg).Background(t.SelBg).Bold(true)
	s.Marker = ns().Foreground(t.Accent).Bold(true)

	s.Brand = ns().Foreground(t.BarBg).Background(t.Accent2).Bold(true).Padding(0, 1)
	s.TabActive = ns().Foreground(t.SelFg).Background(t.Accent).Bold(true).Padding(0, 1)
	s.TabInactive = ns().Foreground(t.Muted).Padding(0, 1)
	s.TabBar = ns().Foreground(t.Faint)

	s.ModePill = ns().Foreground(t.BarBg).Background(t.Accent).Bold(true).Padding(0, 1)
	s.StatusBar = ns().Foreground(t.BarFg).Background(t.BarBg)
	s.Hint = ns().Foreground(t.Muted)
	s.HintKey = ns().Foreground(t.Accent).Bold(true)
	s.ToastInfo = ns().Foreground(t.Info).Background(t.BarBg).Padding(0, 1)
	s.ToastOK = ns().Foreground(t.Good).Background(t.BarBg).Bold(true).Padding(0, 1)
	s.ToastWarn = ns().Foreground(t.Warn).Background(t.BarBg).Bold(true).Padding(0, 1)
	s.ToastErr = ns().Foreground(t.SelFg).Background(t.Bad).Bold(true).Padding(0, 1)

	s.Card = ns().Border(lipgloss.RoundedBorder()).BorderForeground(t.Faint).Padding(0, 1)
	s.CardTitle = ns().Foreground(t.Accent).Bold(true)
	s.Overlay = ns().Border(lipgloss.RoundedBorder()).BorderForeground(t.Accent).Padding(0, 1)

	s.Prompt = ns().Foreground(t.Accent).Bold(true)
	s.Input = ns().Foreground(t.Text)
	s.Placeholder = ns().Foreground(t.Faint)
	s.Error = ns().Foreground(t.Bad).Bold(true)

	s.PillGood = ns().Foreground(t.BarBg).Background(t.Good).Bold(true).Padding(0, 1)
	s.PillBad = ns().Foreground(t.SelFg).Background(t.Bad).Bold(true).Padding(0, 1)
	s.PillMuted = ns().Foreground(t.BarBg).Background(t.Muted).Bold(true).Padding(0, 1)
	return s
}

// status colors a proxy status like px proxies does.
func (s *Styles) status(st string) lipgloss.Style {
	switch st {
	case db.StatusAlive:
		return s.Good
	case db.StatusDead:
		return s.Bad
	default:
		return s.Muted
	}
}

// statusPill renders a status as a colored badge ("[alive]" when plain).
func (s *Styles) statusPill(st string) string {
	if s.plain {
		return "[" + st + "]"
	}
	switch st {
	case db.StatusAlive:
		return s.PillGood.Render(st)
	case db.StatusDead:
		return s.PillBad.Render(st)
	default:
		return s.PillMuted.Render(st)
	}
}

// ms colors a latency: green < 300ms, yellow < 1s, red above, gray unknown.
func (s *Styles) ms(ms int) lipgloss.Style {
	switch {
	case ms <= 0:
		return s.Muted
	case ms < 300:
		return s.Good
	case ms < 1000:
		return s.Warn
	default:
		return s.Bad
	}
}

// rate colors a 0-1 success rate.
func (s *Styles) rate(r float64) lipgloss.Style {
	switch {
	case r < 0:
		return s.Muted
	case r >= 0.9:
		return s.Good
	case r >= 0.6:
		return s.Warn
	default:
		return s.Bad
	}
}

// scanState colors a scan run state.
func (s *Styles) scanState(st string) lipgloss.Style {
	switch st {
	case db.ScanDone:
		return s.Good
	case db.ScanRunning:
		return s.Info
	case db.ScanPaused:
		return s.Warn
	case db.ScanFailed:
		return s.Bad
	default:
		return s.Muted
	}
}

// anon colors an anonymity grade.
func (s *Styles) anon(a string) lipgloss.Style {
	switch a {
	case "elite":
		return s.Good
	case "anonymous":
		return s.Info
	case "transparent":
		return s.Bad
	default:
		return s.Muted
	}
}

// score colors a 0-100 score.
func (s *Styles) score(f *float64) lipgloss.Style {
	if f == nil {
		return s.Muted
	}
	return s.rate(*f / 100)
}
