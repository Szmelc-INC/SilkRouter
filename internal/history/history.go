// Package history holds the helpers of the usage log that are not database
// queries: classifying a routed program's exit code, finding the terminal a
// command runs on, the plain-text log in <home>/logs/usage.log, and parsing
// time windows ("7d", "2026-11-01").
package history

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

// exitClasses maps well-known programs' exit codes to err_class values, so
// the log can tell "the proxy timed out" from "the program failed".
var exitClasses = map[string]map[int]string{
	"curl": {
		5: db.ErrDNS, 6: db.ErrDNS, 7: db.ErrRefused, 28: db.ErrTimeout, 35: db.ErrTLS, 51: db.ErrTLS,
		52: db.ErrReset, 55: db.ErrReset, 56: db.ErrReset, 58: db.ErrTLS, 59: db.ErrTLS, 60: db.ErrTLS,
		67: db.ErrAuth, 77: db.ErrTLS, 80: db.ErrTLS, 83: db.ErrTLS, 90: db.ErrTLS, 91: db.ErrTLS,
		22: db.ErrHTTPStatus, 97: db.ErrProto,
	},
	"wget":  {4: db.ErrReset, 5: db.ErrTLS, 6: db.ErrAuth, 8: db.ErrHTTPStatus},
	"wget2": {4: db.ErrReset, 5: db.ErrTLS, 6: db.ErrAuth, 8: db.ErrHTTPStatus},
	"ssh":   {255: db.ErrRefused},
	"scp":   {255: db.ErrRefused},
	"sftp":  {255: db.ErrRefused},
	"aria2c": {
		2: db.ErrTimeout, 6: db.ErrReset, 19: db.ErrDNS, 22: db.ErrHTTPStatus, 24: db.ErrAuth,
	},
	"http":  {2: db.ErrTimeout, 3: db.ErrHTTPStatus, 4: db.ErrHTTPStatus, 5: db.ErrHTTPStatus, 6: db.ErrProto},
	"https": {2: db.ErrTimeout, 3: db.ErrHTTPStatus, 4: db.ErrHTTPStatus, 5: db.ErrHTTPStatus, 6: db.ErrProto},
}

// ClassifyExit returns the err_class of a finished command ("" for exit 0).
// Known programs (curl, wget, ssh, aria2c, httpie) map their network exit
// codes; 127 is "not found" (other), 130 / 143 interrupted (other); anything
// else is exit_nonzero.
func ClassifyExit(cmd string, code int) string {
	if code == 0 {
		return ""
	}
	base := filepath.Base(strings.ReplaceAll(cmd, `\`, "/"))
	base = strings.TrimSuffix(base, ".exe")
	if m, ok := exitClasses[base]; ok {
		if c, ok := m[code]; ok {
			return c
		}
	}
	switch code {
	case 126, 127, 130, 143:
		return db.ErrOther
	}
	return db.ErrExitNonzero
}

// TTY returns the terminal of stdin ("/dev/pts/3"), or "" when stdin is not
// a terminal or the platform cannot tell.
func TTY() string {
	if v := os.Getenv("SILKROUTER_TTY"); v != "" {
		return v
	}
	if runtime.GOOS == "linux" {
		if p, err := os.Readlink("/proc/self/fd/0"); err == nil && strings.HasPrefix(p, "/dev/") {
			return p
		}
		return ""
	}
	if v := os.Getenv("TTY"); strings.HasPrefix(v, "/dev/") {
		return v
	}
	return ""
}

// Line renders one usage row for the text log.
func Line(u db.Usage) string {
	code := "-"
	if u.ExitCode != nil {
		code = strconv.Itoa(*u.ExitCode)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %-28s exit=%-3s dur=%dms", u.TSStart.UTC().Format(time.RFC3339), u.Mode, dash(u.ProxyURL), code, u.DurationMS)
	if u.MS > 0 {
		fmt.Fprintf(&b, " ms=%d", u.MS)
	}
	if u.PreflightOK != nil {
		fmt.Fprintf(&b, " preflight=%v", *u.PreflightOK)
	}
	if u.ErrClass != "" {
		fmt.Fprintf(&b, " err=%s", u.ErrClass)
	}
	if u.SessionID != "" {
		fmt.Fprintf(&b, " session=%s", u.SessionID)
	}
	if u.TTY != "" {
		fmt.Fprintf(&b, " tty=%s", u.TTY)
	}
	if u.Cwd != "" {
		fmt.Fprintf(&b, " cwd=%s", strconv.Quote(u.Cwd))
	}
	fmt.Fprintf(&b, " cmd=%s", strconv.Quote(strings.Join(u.Argv, " ")))
	return b.String()
}

// AppendTextLog appends Line(u) to path (created 0600).
func AppendTextLog(path string, u db.Usage) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, Line(u))
	return err
}

// ParseSince parses a time window: a duration back from now ("90m", "36h",
// "7d", "2w", "1y" or a Go duration) or a date / time ("2026-11-01",
// "2026-11-01 14:00", RFC 3339). Empty = zero time.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q (want e.g. 30m, 12h, 7d, 2w or 2026-11-01)", s)
}

// ParseDuration is time.ParseDuration plus d (days), w (weeks) and y
// (365 days) suffixes, e.g. "7d", "1w2d", "1.5d".
func ParseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	var total time.Duration
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] == '.' || (rest[i] >= '0' && rest[i] <= '9')) {
			i++
		}
		if i == 0 || i == len(rest) {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		n, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		j := i
		for j < len(rest) && (rest[j] < '0' || rest[j] > '9') && rest[j] != '.' {
			j++
		}
		unit := rest[i:j]
		var u time.Duration
		switch unit {
		case "y":
			u = 365 * 24 * time.Hour
		case "w":
			u = 7 * 24 * time.Hour
		case "d":
			u = 24 * time.Hour
		case "h":
			u = time.Hour
		case "m":
			u = time.Minute
		case "s":
			u = time.Second
		case "ms":
			u = time.Millisecond
		default:
			return 0, fmt.Errorf("bad duration unit %q in %q", unit, s)
		}
		total += time.Duration(n * float64(u))
		rest = rest[j:]
	}
	return total, nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
