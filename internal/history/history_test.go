package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

func TestClassifyExit(t *testing.T) {
	cases := []struct {
		cmd  string
		code int
		want string
	}{
		{"curl", 0, ""},
		{"/usr/bin/curl", 7, db.ErrRefused},
		{"curl", 28, db.ErrTimeout},
		{"curl.exe", 35, db.ErrTLS},
		{"curl", 3, db.ErrExitNonzero},
		{"wget", 4, db.ErrReset},
		{"ssh", 255, db.ErrRefused},
		{"git", 128, db.ErrExitNonzero},
		{"anything", 127, db.ErrOther},
		{"anything", 130, db.ErrOther},
	}
	for _, c := range cases {
		if got := ClassifyExit(c.cmd, c.code); got != c.want {
			t.Errorf("%s %d: %q want %q", c.cmd, c.code, got, c.want)
		}
	}
}

func TestParseSinceAndDuration(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Duration{
		"90m": 90 * time.Minute, "36h": 36 * time.Hour, "7d": 7 * 24 * time.Hour, "2w": 14 * 24 * time.Hour,
		"1w2d": 9 * 24 * time.Hour, "1.5d": 36 * time.Hour, "1h30m": 90 * time.Minute,
	} {
		got, err := ParseSince(in, now)
		if err != nil || now.Sub(got) != want {
			t.Errorf("%s: %v %v", in, now.Sub(got), err)
		}
	}
	got, err := ParseSince("2026-11-01", now)
	if err != nil || got.Day() != 1 || got.Month() != 11 {
		t.Fatalf("date: %v %v", got, err)
	}
	if _, err := ParseSince("2026-11-01 14:30", now); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"soon", "7x", "d", "12"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if z, err := ParseSince("", now); err != nil || !z.IsZero() {
		t.Fatal("empty")
	}
}

func TestTextLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "usage.log")
	code := 7
	ok := true
	u := db.Usage{TSStart: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC), Mode: db.ModeRoute,
		ProxyURL: "socks5://192.0.2.1:1080", Argv: []string{"curl", "-s", "https://example.com"}, ExitCode: &code,
		DurationMS: 1234, MS: 88, PreflightOK: &ok, ErrClass: db.ErrRefused, Cwd: "/home/x", TTY: "/dev/pts/1", SessionID: "abc"}
	if err := AppendTextLog(path, u); err != nil {
		t.Fatal(err)
	}
	if err := AppendTextLog(path, u); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%q", data)
	}
	for _, want := range []string{"2026-11-14T10:00:00Z", "route", "socks5://192.0.2.1:1080", "exit=7", "dur=1234ms",
		"ms=88", "preflight=true", "err=refused", "session=abc", "tty=/dev/pts/1", `cwd="/home/x"`, `cmd="curl -s https://example.com"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line lacks %q: %s", want, lines[0])
		}
	}
}
