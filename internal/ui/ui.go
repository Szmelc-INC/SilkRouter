// Package ui centralises terminal colour handling so every subcommand renders
// output consistently and degrades cleanly when the stream is not a TTY.
// Everything here is pure standard library and works on Linux and Windows.
//
// The colour mode is "auto" (colour only on a terminal, never with NO_COLOR
// or TERM=dumb), "always" or "never". --no-color and NO_COLOR win over the
// config file's ui.color.
package ui

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Palette holds ANSI escape sequences. When colour is disabled every field is
// an empty string, so callers can interpolate them unconditionally.
type Palette struct {
	Red, Green, Yellow, Blue, Magenta, Cyan, Gray string
	Dim, Bold, Italic, Underline, Reset           string
}

// Colour modes.
const (
	ModeAuto   = "auto"
	ModeAlways = "always"
	ModeNever  = "never"
)

var (
	mode = ModeAuto

	colorOn = Palette{
		Red:       "\033[31m",
		Green:     "\033[32m",
		Yellow:    "\033[33m",
		Blue:      "\033[34m",
		Magenta:   "\033[35m",
		Cyan:      "\033[36m",
		Gray:      "\033[90m",
		Dim:       "\033[2m",
		Bold:      "\033[1m",
		Italic:    "\033[3m",
		Underline: "\033[4m",
		Reset:     "\033[0m",
	}
	colorOff = Palette{}
)

// SetMode selects the colour mode (auto, always or never). Unknown values
// fall back to auto.
func SetMode(m string) {
	switch m {
	case ModeAlways, ModeNever:
		mode = m
	default:
		mode = ModeAuto
	}
}

// Mode returns the active colour mode.
func Mode() string { return mode }

// SetEnabled forces colour on or off.
func SetEnabled(on bool) {
	if on {
		mode = ModeAlways
	} else {
		mode = ModeNever
	}
}

// Enabled reports whether general output (both stdout and stderr) is
// coloured.
func Enabled() bool { return enabledFor(os.Stdout) && enabledFor(os.Stderr) }

// EnabledFor reports whether output written to w is coloured: always in
// "always" mode, else only when w is a terminal and NO_COLOR is unset.
func EnabledFor(w io.Writer) bool { return enabledFor(w) }

func enabledFor(w io.Writer) bool {
	switch mode {
	case ModeAlways:
		return true
	case ModeNever:
		return false
	}
	if NoColorEnv() || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && IsTerminal(f)
}

// NoColorEnv reports whether NO_COLOR is set (to any value, per no-color.org).
func NoColorEnv() bool {
	_, ok := os.LookupEnv("NO_COLOR")
	return ok
}

// Colors returns the palette for general output (stdout and stderr). In
// auto mode it is coloured only when both streams are terminals, so piping
// or redirecting either one yields plain text.
func Colors() Palette {
	if Enabled() {
		return colorOn
	}
	return colorOff
}

// ColorsFor returns the palette for output written to w.
func ColorsFor(w io.Writer) Palette {
	if enabledFor(w) {
		return colorOn
	}
	return colorOff
}

// IsTerminal reports whether f is a character device (a TTY), using only the
// standard library so it stays cross-platform and dependency-free.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Width returns the terminal width from $COLUMNS, else def.
func Width(def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS"))); err == nil && n > 20 {
		return n
	}
	return def
}

// Wrap colours s with the given code when general output is coloured.
func Wrap(code, s string) string {
	if !Enabled() || code == "" {
		return s
	}
	return code + s + colorOn.Reset
}

// Errf prints a formatted line to stderr.
func Errf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format, a...)
}
