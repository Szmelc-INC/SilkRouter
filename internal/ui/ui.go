// Package ui centralises terminal colour handling so every subcommand renders
// output consistently and degrades cleanly when the stream is not a TTY.
// Everything here is pure standard library and works on Linux and Windows.
package ui

import (
	"fmt"
	"os"
)

// ANSI colour codes. When colour is disabled these become empty strings.
type Palette struct {
	Red, Green, Yellow, Cyan, Dim, Bold, Reset string
}

var (
	enabled = detectColor()

	colorOn = Palette{
		Red:    "\033[0;31m",
		Green:  "\033[0;32m",
		Yellow: "\033[0;33m",
		Cyan:   "\033[0;36m",
		Dim:    "\033[2m",
		Bold:   "\033[1m",
		Reset:  "\033[0m",
	}
	colorOff = Palette{}
)

// SetEnabled forces colour on or off (used by --no-color flags).
func SetEnabled(on bool) { enabled = on }

// Colors returns the active palette.
func Colors() Palette {
	if enabled {
		return colorOn
	}
	return colorOff
}

// detectColor honours NO_COLOR and only enables colour on a real terminal.
func detectColor() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return isTerminal(os.Stderr)
}

// isTerminal reports whether f is a character device (a TTY), using only the
// standard library so it stays cross-platform and dependency-free.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Wrap colours s with the given code when colour is enabled.
func Wrap(code, s string) string {
	if !enabled {
		return s
	}
	return code + s + colorOn.Reset
}

// Errf prints a formatted error line to stderr.
func Errf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format, a...)
}
