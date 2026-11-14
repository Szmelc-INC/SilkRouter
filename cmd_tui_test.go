package main

import (
	"strings"
	"testing"
)

func TestTUICommand(t *testing.T) {
	sandbox(t)
	code, out, _ := runApp(t, "px", "tui", "--help")
	for _, want := range []string{"px tui [options] [search...]", "aliases: ui", "--no-alt-screen", "--no-mouse",
		"--refresh DUR", "--theme NAME", "-t, --tab TAB", "tui.start_tab", "tui.alt_screen", "-S, --search TEXT",
		"command palette"} {
		if !strings.Contains(out, want) {
			t.Errorf("tui --help misses %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatalf("tui --help: exit %d", code)
	}
	if _, out, _ = runApp(t, "px", "help", "ui"); !strings.Contains(out, "px tui") {
		t.Fatalf("help ui:\n%s", out)
	}

	// Bad flags are usage errors, checked before anything else.
	for _, args := range [][]string{{"--tab", "nope"}, {"--theme", "plaid"}, {"--sort", "speed"}, {"--status", "zombie"}} {
		code, _, errs := runApp(t, append([]string{"px", "ui"}, args...)...)
		if code != 2 || !strings.Contains(errs, "unknown") {
			t.Errorf("%v: exit %d %s", args, code, errs)
		}
	}

	// Without a terminal it explains instead of hanging.
	code, _, errs := runApp(t, "px", "tui", "--tab", "proxies")
	if code != 1 || !strings.Contains(errs, "interactive terminal") {
		t.Fatalf("no terminal: exit %d %s", code, errs)
	}
}
