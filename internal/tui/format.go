package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// ago renders a compact age ("5s", "3m", "2h", "4d"); "-" for zero.
func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// msText is a latency without unit ("-" when unknown), like px proxies.
func msText(ms int) string {
	if ms <= 0 {
		return "-"
	}
	return strconv.Itoa(ms)
}

// msUnit is a latency with unit ("120ms", "-").
func msUnit(ms int) string {
	if ms <= 0 {
		return "-"
	}
	return strconv.Itoa(ms) + "ms"
}

// durText renders a duration in milliseconds compactly.
func durText(ms int64) string {
	switch {
	case ms <= 0:
		return "-"
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm%02ds", ms/60_000, (ms/1000)%60)
	default:
		return fmt.Sprintf("%dh%02dm", ms/3_600_000, (ms/60_000)%60)
	}
}

// commas formats n with thousands separators (12,345).
func commas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// plural is "1 proxy" / "12,345 proxies".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return commas(n) + " " + many
}

// pctText renders a 0-1 rate as a percentage ("-" for a negative rate).
func pctText(r float64) string {
	if r < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*r)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fptr(f *float64) string {
	if f == nil {
		return "-"
	}
	return strconv.FormatFloat(*f, 'f', 0, 64)
}

func iptr(i *int) string {
	if i == nil {
		return "-"
	}
	return strconv.Itoa(*i)
}

// bar is a horizontal gauge of width cells filled to frac (0-1).
func bar(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	frac = min(max(frac, 0), 1)
	n := int(frac*float64(width) + 0.5)
	if n == 0 && frac > 0 {
		n = 1
	}
	return strings.Repeat("█", n) + strings.Repeat("░", width-n)
}

// truncate shortens s to w cells, ending with an ellipsis when cut.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// pad truncates or pads s to exactly w cells.
func pad(s string, w int, right bool) string {
	s = truncate(s, w)
	n := w - ansi.StringWidth(s)
	if n <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", n) + s
	}
	return s + strings.Repeat(" ", n)
}

// fitLines makes s exactly h lines, none wider than w cells.
func fitLines(s string, w, h int) string {
	if h <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			lines[i] = ansi.Truncate(l, w, "")
		}
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func tsShort(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("01-02 15:04:05")
}

func tsLong(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05") + " (" + ago(t) + " ago)"
}

// cycle returns the element after (step 1) or before (step -1) cur in
// list, wrapping around; the first element when cur is not in list.
func cycle(list []string, cur string, step int) string {
	for i, v := range list {
		if v == cur {
			return list[((i+step)%len(list)+len(list))%len(list)]
		}
	}
	return list[0]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
