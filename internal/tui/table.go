package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// column describes one table column.
type column struct {
	title string
	right bool // right-aligned (numbers)
	max   int  // widest the column gets (0 = no limit)
	// flex columns (tags, command lines) give up width first when the
	// table does not fit.
	flex bool
	min  int // narrowest a flex column shrinks to (0 = its title)
}

// cell is one table cell: plain text plus the style wrapped around it
// after padding (widths are computed on the plain text).
type cell struct {
	text  string
	style lipgloss.Style
}

func plainCell(s string) cell { return cell{text: s} }

// table is a scrollable, cursor-driven table with per-cell colors. It is
// shared by every list view; rows are rebuilt from the data whenever it
// or the theme changes.
type table struct {
	cols     []column
	rows     [][]cell
	natural  []int // widest plain text per column (cached by setRows)
	cursor   int
	offset   int
	height   int // visible rows (the header excluded)
	empty    string
	noCursor bool // a static table (detail pane): no selection
}

const colGap = 2

func (t *table) setColumns(cols []column) {
	t.cols = cols
	t.measure()
}

func (t *table) setRows(rows [][]cell) {
	t.rows = rows
	t.measure()
	t.clamp()
}

func (t *table) measure() {
	t.natural = make([]int, len(t.cols))
	for i, c := range t.cols {
		t.natural[i] = ansi.StringWidth(c.title)
	}
	for _, r := range t.rows {
		for i := 0; i < len(r) && i < len(t.cols); i++ {
			if w := ansi.StringWidth(r[i].text); w > t.natural[i] {
				t.natural[i] = w
			}
		}
	}
}

// setHeight sets the table's total height (header included).
func (t *table) setHeight(h int) {
	t.height = max(1, h-1)
	t.clamp()
}

func (t *table) clamp() {
	if t.cursor >= len(t.rows) {
		t.cursor = len(t.rows) - 1
	}
	if t.cursor < 0 {
		t.cursor = 0
	}
	if t.offset > t.cursor {
		t.offset = t.cursor
	}
	if t.height > 0 && t.cursor >= t.offset+t.height {
		t.offset = t.cursor - t.height + 1
	}
	if maxOff := max(0, len(t.rows)-t.height); t.offset > maxOff {
		t.offset = maxOff
	}
	if t.offset < 0 {
		t.offset = 0
	}
}

func (t *table) move(n int) { t.cursor += n; t.clamp() }
func (t *table) top()       { t.cursor = 0; t.clamp() }
func (t *table) bottom()    { t.cursor = len(t.rows) - 1; t.clamp() }
func (t *table) page(n int) { t.move(n * max(1, t.height)) }
func (t *table) half(n int) { t.move(n * max(1, t.height/2)) }
func (t *table) reset()     { t.cursor, t.offset = 0, 0 }
func (t *table) len() int   { return len(t.rows) }
func (t *table) selected() int {
	if len(t.rows) == 0 {
		return -1
	}
	return t.cursor
}

// rowAt maps a line of the rendered table (0 = header) to a row index,
// or -1.
func (t *table) rowAt(y int) int {
	if y < 1 || y-1 >= t.height {
		return -1
	}
	i := t.offset + y - 1
	if i >= len(t.rows) {
		return -1
	}
	return i
}

// layout fits the columns into width: flex columns shrink first, then
// columns are dropped from the right (never the first two), then the last
// visible one is cut. A width of 0 hides a column.
func (t *table) layout(width int) []int {
	n := len(t.cols)
	ws := make([]int, n)
	for i, c := range t.cols {
		w := t.natural[i]
		if c.max > 0 {
			w = min(w, max(c.max, ansi.StringWidth(c.title)))
		}
		ws[i] = w
	}
	avail := width - 2 // selection marker
	total := func() int {
		s, k := 0, 0
		for _, w := range ws {
			if w > 0 {
				s += w
				k++
			}
		}
		if k > 1 {
			s += colGap * (k - 1)
		}
		return s
	}
	for i := n - 1; i >= 0; i-- { // the rightmost flex column gives first
		if c := t.cols[i]; c.flex {
			if over := total() - avail; over > 0 {
				floor := max(c.min, ansi.StringWidth(c.title))
				ws[i] = max(floor, ws[i]-over)
			}
		}
	}
	for i := n - 1; i >= 2 && total() > avail; i-- {
		ws[i] = 0
	}
	if over := total() - avail; over > 0 {
		for i := n - 1; i >= 0; i-- {
			if ws[i] > 0 {
				ws[i] = max(1, ws[i]-over)
				break
			}
		}
	}
	return ws
}

// view renders the header and the visible rows.
func (t *table) view(s *Styles, width int) string {
	ws := t.layout(width)
	full := 0
	k := 0
	for _, w := range ws {
		if w > 0 {
			full += w
			k++
		}
	}
	if k > 1 {
		full += colGap * (k - 1)
	}
	var b strings.Builder
	b.WriteString("  ")
	first := true
	for i, c := range t.cols {
		if ws[i] == 0 {
			continue
		}
		if !first {
			b.WriteString(strings.Repeat(" ", colGap))
		}
		first = false
		b.WriteString(s.Header.Render(pad(c.title, ws[i], c.right)))
	}
	if len(t.rows) == 0 {
		if t.empty != "" {
			b.WriteString("\n  " + s.Muted.Render(t.empty))
		}
		return b.String()
	}
	end := min(len(t.rows), t.offset+max(1, t.height))
	for r := t.offset; r < end; r++ {
		b.WriteByte('\n')
		row := t.rows[r]
		sel := r == t.cursor && !t.noCursor
		if sel {
			var line strings.Builder
			first := true
			for i := range t.cols {
				if ws[i] == 0 {
					continue
				}
				if !first {
					line.WriteString(strings.Repeat(" ", colGap))
				}
				first = false
				text := ""
				if i < len(row) {
					text = row[i].text
				}
				line.WriteString(pad(text, ws[i], t.cols[i].right))
			}
			b.WriteString(s.Marker.Render("▸ "))
			b.WriteString(s.Selected.Render(pad(line.String(), full, false)))
			continue
		}
		b.WriteString("  ")
		first := true
		for i := range t.cols {
			if ws[i] == 0 {
				continue
			}
			if !first {
				b.WriteString(strings.Repeat(" ", colGap))
			}
			first = false
			var c cell
			if i < len(row) {
				c = row[i]
			}
			b.WriteString(c.style.Render(pad(c.text, ws[i], t.cols[i].right)))
		}
	}
	return b.String()
}

// position is "12/500" for the status bar.
func (t *table) position() string {
	if len(t.rows) == 0 {
		return "0/0"
	}
	return commas(t.cursor+1) + "/" + commas(len(t.rows))
}
