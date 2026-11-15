package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/szmelc-inc/silkrouter/internal/db"
)

// detailData is everything 'px show' prints about one proxy.
type detailData struct {
	p      *db.Proxy
	ms     []db.Measurement
	rep    []db.Reputation
	tags   []db.Tag
	uses   int
	usage  []db.Usage
	byKind map[string][2]int // kind -> [ok, fail] in the shown window
}

type detailMsg struct {
	id   int64
	seq  int
	data *detailData
	err  error
}

// detailView is the details pane of the Proxies tab (Enter on a row).
type detailView struct {
	id   int64
	seq  int
	data *detailData
	err  error
	vp   viewport.Model
	w, h int
}

func newDetailView(id int64) *detailView {
	return &detailView{id: id, vp: viewport.New(80, 20)}
}

func (v *detailView) load(m *Model) tea.Cmd {
	v.seq++
	seq, id := v.seq, v.id
	d, ctx := m.d, m.ctx
	recent := m.cfg.DB.ShowRecent
	if recent <= 0 {
		recent = 15
	}
	return func() tea.Msg {
		p, err := d.GetProxy(ctx, id)
		if err != nil {
			return detailMsg{id: id, seq: seq, err: err}
		}
		out := &detailData{p: p, byKind: map[string][2]int{}}
		if out.ms, err = d.Measurements(ctx, id, recent); err != nil {
			return detailMsg{id: id, seq: seq, err: err}
		}
		ips := []string{p.Host}
		if p.ExitIP != "" && p.ExitIP != p.Host {
			ips = append(ips, p.ExitIP)
		}
		out.rep, _ = d.ReputationFor(ctx, ips...)
		out.tags, _ = d.Tags(ctx, id)
		for _, t := range out.tags {
			p.Tags = append(p.Tags, t.Label())
		}
		out.uses, _ = d.UsageCount(ctx, id)
		out.usage, _ = d.ListUsage(ctx, db.UsageFilter{ProxyID: id, Limit: 8})
		for _, ms := range out.ms {
			c := out.byKind[ms.Kind]
			if ms.OK {
				c[0]++
			} else {
				c[1]++
			}
			out.byKind[ms.Kind] = c
		}
		return detailMsg{id: id, seq: seq, data: out}
	}
}

func (v *detailView) apply(m *Model, msg detailMsg) {
	if msg.id != v.id || msg.seq != v.seq {
		return
	}
	v.err = msg.err
	if msg.data != nil {
		v.data = msg.data
	}
	v.rebuild(m)
}

func (v *detailView) resize(m *Model, w, h int) {
	v.w, v.h = w, h
	v.vp.Width, v.vp.Height = w, h
	v.rebuild(m)
}

func (v *detailView) rebuild(m *Model) {
	off := v.vp.YOffset
	v.vp.SetContent(v.content(m))
	v.vp.SetYOffset(off)
}

func (v *detailView) scroll(d int) {
	if d < 0 {
		v.vp.ScrollUp(-d)
	} else {
		v.vp.ScrollDown(d)
	}
}

// key handles the pane's own keys; ok=false passes the key on (actions).
func (v *detailView) key(m *Model, pv *proxiesView, msg tea.KeyMsg) (tea.Cmd, bool) {
	switch {
	case key.Matches(msg, keys.Back):
		pv.detail = nil
		return nil, true
	case key.Matches(msg, keys.Reload):
		return v.load(m), true
	case key.Matches(msg, keys.Up):
		v.vp.ScrollUp(1)
	case key.Matches(msg, keys.Down):
		v.vp.ScrollDown(1)
	case key.Matches(msg, keys.PageUp):
		v.vp.PageUp()
	case key.Matches(msg, keys.PageDown):
		v.vp.PageDown()
	case key.Matches(msg, keys.HalfUp):
		v.vp.HalfPageUp()
	case key.Matches(msg, keys.HalfDown):
		v.vp.HalfPageDown()
	case key.Matches(msg, keys.Top):
		v.vp.GotoTop()
	case key.Matches(msg, keys.Bottom):
		v.vp.GotoBottom()
	default:
		return nil, false
	}
	return nil, true
}

func (v *detailView) render(m *Model) string {
	return v.vp.View()
}

func (v *detailView) status(m *Model) []string {
	segs := []string{"proxy #" + strconv.FormatInt(v.id, 10)}
	if v.data != nil {
		segs = append(segs, v.data.p.Key())
	}
	if v.vp.TotalLineCount() > v.vp.Height {
		segs = append(segs, fmt.Sprintf("%.0f%%", 100*v.vp.ScrollPercent()))
	}
	return segs
}

// content renders the pane: header, two field columns, then the
// measurement, reputation and usage tables.
func (v *detailView) content(m *Model) string {
	s := m.styles
	if v.err != nil {
		return s.Error.Render("  " + v.err.Error())
	}
	if v.data == nil {
		return s.Muted.Render("  loading proxy #" + strconv.FormatInt(v.id, 10) + "…")
	}
	o := v.data
	p := o.p
	w := max(40, v.w)
	var b strings.Builder

	head := " " + s.Bold.Render(p.Key()) + "  " + s.Muted.Render("#"+strconv.FormatInt(p.ID, 10)) + "  " + s.statusPill(p.Status)
	if p.TLSMITM != nil && *p.TLSMITM {
		if s.plain {
			head += "  [MITM]"
		} else {
			head += "  " + s.PillBad.Render("MITM")
		}
	}
	if p.Rank != nil {
		head += "  " + s.Accent.Render("rank "+iptr(p.Rank))
	}
	b.WriteString(head + "\n")
	b.WriteString(s.Faint.Render(" "+strings.Repeat("─", w-2)) + "\n")

	type kv struct{ k, v string }
	label := func(k string) string { return s.Accent.Render(pad(k, 14, false)) }
	block := func(rows []kv) string {
		var lb strings.Builder
		for i, r := range rows {
			if i > 0 {
				lb.WriteByte('\n')
			}
			lb.WriteString(" " + label(r.k) + " " + r.v)
		}
		return lb.String()
	}
	avg := "-"
	if p.AvgMS > 0 {
		avg = fmt.Sprintf("%.1fms over %d timed", p.AvgMS, p.MSCount)
	}
	rate := pctText(p.SuccessRate())
	left := []kv{
		{"source", dash(p.Source)},
		{"added", tsLong(p.AddedAt)},
		{"seen in source", tsLong(p.LastSeenInSource)},
		{"checked", tsLong(p.CheckedAt)},
		{"latency", "last " + s.ms(p.LastMS).Render(msUnit(p.LastMS)) + "  best " + s.ms(p.BestMS).Render(msUnit(p.BestMS)) + "  avg " + avg},
		{"checks", s.Good.Render(strconv.Itoa(p.OKCount)+" ok") + " / " + s.Bad.Render(strconv.Itoa(p.FailCount)+" failed") +
			" (" + s.rate(p.SuccessRate()).Render(rate) + ")"},
		{"streak", fmt.Sprintf("%d ok, %d failed", p.OKStreak, p.FailStreak)},
	}
	if p.Username != "" {
		left = append([]kv{{"auth", p.Username + ":***"}}, left...)
	}
	if p.JitterMS != nil {
		left = append(left, kv{"jitter", fmt.Sprintf("%.1fms", *p.JitterMS)})
	}
	geo := dash(p.Country)
	if p.Region != "" || p.City != "" {
		geo += " " + strings.TrimSpace(p.Region+" "+p.City)
	}
	asn := strings.TrimSpace(dash(p.ASN) + " " + p.Org)
	listed := s.Good.Render("0 lists")
	if p.ListedCount > 0 {
		listed = s.Bad.Render(fmt.Sprintf("%d list(s)", p.ListedCount))
	}
	right := []kv{
		{"exit ip", dash(p.ExitIP)},
		{"country", geo},
		{"asn / org", asn},
		{"anonymity", s.anon(p.Anonymity).Render(dash(p.Anonymity))},
		{"listed on", listed},
	}
	right = append(right, kv{"https / tls", tlsLine(s, p)})
	if p.TLSMITM != nil && *p.TLSMITM {
		right = append(right, kv{"tls issuer", s.Bad.Render(dash(p.TLSIssuer))})
	}
	if len(p.Protocols) > 0 {
		right = append(right, kv{"protocols", strings.Join(p.Protocols, " ")})
	}
	if p.Hosting != nil && *p.Hosting {
		right = append(right, kv{"hosting", s.Warn.Render("datacenter / hosting range")})
	}
	if !p.FullScanAt.IsZero() {
		right = append(right, kv{"full scan", ago(p.FullScanAt) + " ago"})
	}
	scoreLine := func(f *float64) string {
		if f == nil {
			return s.Muted.Render("-")
		}
		return s.score(f).Render(bar(*f/100, 10) + " " + fptr(f))
	}
	scores := []kv{
		{"speed", scoreLine(p.SpeedScore)},
		{"clean", scoreLine(p.CleanScore)},
		{"stable", scoreLine(p.StableScore)},
	}
	usage := fmt.Sprintf("%d uses, %d ok, %d failed, %d proxy errors", p.UseCount, p.UseOK, p.UseFail, p.UseProxyErr)
	if !p.LastUsedAt.IsZero() {
		usage += fmt.Sprintf(" · last %s %s ago", dash(p.LastCmd), ago(p.LastUsedAt))
	}
	var tags []string
	for _, t := range o.tags {
		tags = append(tags, s.tagStyle(t.Label()).Render(t.Label()))
	}
	tagLine := s.Muted.Render("none (t adds one)")
	if len(tags) > 0 {
		tagLine = strings.Join(tags, " ")
	}

	lb, rb := block(left), block(append(right, scores...))
	if w >= 110 {
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, lb, "    ", rb))
	} else {
		b.WriteString(lb + "\n" + rb)
	}
	b.WriteString("\n" + block([]kv{{"usage", usage}, {"tags", tagLine}}) + "\n")

	// Recent measurements.
	var kinds []string
	for _, k := range db.Kinds {
		if c, ok := o.byKind[k]; ok {
			kinds = append(kinds, fmt.Sprintf("%s %d/%d", k, c[0], c[0]+c[1]))
		}
	}
	b.WriteString("\n " + s.Title.Render("Recent measurements") + " " +
		s.Muted.Render(fmt.Sprintf("(%d shown%s)", len(o.ms), prefixed(" · ", strings.Join(kinds, ", ")))) + "\n")
	mt := table{noCursor: true, empty: "none yet (c checks the proxy now)"}
	mt.setColumns([]column{{title: "TIME"}, {title: "KIND"}, {title: "PROBE"}, {title: "RESULT"},
		{title: "MS", right: true}, {title: "DETAIL", flex: true}})
	var rows [][]cell
	for _, ms := range o.ms {
		res := cell{"ok", s.Good}
		if !ms.OK {
			res = cell{"FAIL", s.Bad}
		}
		extra := ms.ErrClass
		if ms.ExitIP != "" {
			extra = strings.TrimSpace(extra + " exit " + ms.ExitIP)
		}
		rows = append(rows, []cell{{tsShort(ms.TS), s.Muted}, plainCell(ms.Kind), {dash(ms.Probe), s.Info}, res,
			{msText(ms.MS), s.ms(ms.MS)}, {extra, s.Muted}})
	}
	mt.setRows(rows)
	mt.setHeight(len(rows) + 1)
	b.WriteString(mt.view(s, w) + "\n")

	if len(o.rep) > 0 {
		b.WriteString("\n " + s.Title.Render("Reputation") + " " + s.Muted.Render(fmt.Sprintf("(%d verdicts)", len(o.rep))) + "\n")
		rt := table{noCursor: true}
		rt.setColumns([]column{{title: "VERDICT"}, {title: "IP"}, {title: "LIST"}, {title: "KIND"}, {title: "CHECKED", right: true}})
		var rrows [][]cell
		for _, r := range o.rep {
			verdict := cell{"clean", s.Good}
			if r.Listed {
				verdict = cell{"LISTED", s.Bad}
			}
			rrows = append(rrows, []cell{verdict, plainCell(r.IP), {r.ListName, s.Info}, {r.ListKind, s.Muted}, {ago(r.CheckedAt), s.Muted}})
		}
		rt.setRows(rrows)
		rt.setHeight(len(rrows) + 1)
		b.WriteString(rt.view(s, w) + "\n")
	}

	b.WriteString("\n " + s.Title.Render("Usage") + " " + s.Muted.Render(fmt.Sprintf("(%d commands)", o.uses)) + "\n")
	ut := table{noCursor: true, empty: "never used"}
	ut.setColumns([]column{{title: "TIME"}, {title: "MODE"}, {title: "EXIT", right: true}, {title: "TOOK", right: true}, {title: "COMMAND", flex: true}})
	var urows [][]cell
	for _, u := range o.usage {
		code, cc := "…", s.Info
		if u.ExitCode != nil {
			code, cc = strconv.Itoa(*u.ExitCode), s.Good
			if *u.ExitCode != 0 {
				cc = s.Bad
			}
		}
		urows = append(urows, []cell{{tsShort(u.TSStart), s.Muted}, plainCell(u.Mode), {code, cc}, plainCell(durText(u.DurationMS)),
			plainCell(strings.Join(u.Argv, " "))})
	}
	ut.setRows(urows)
	ut.setHeight(len(urows) + 1)
	b.WriteString(ut.view(s, w))
	return b.String()
}

// tlsLine is the verdict of the full scan's HTTPS check: intercepted (a
// certificate that does not verify: the proxy can read HTTPS), ok, failed
// or not checked.
func tlsLine(s *Styles, p *db.Proxy) string {
	switch {
	case p.TLSMITM != nil && *p.TLSMITM:
		return s.Bad.Bold(true).Render("INTERCEPTED") + s.Muted.Render(" (it decrypts HTTPS)")
	case p.HTTPSOK != nil && *p.HTTPSOK:
		return s.Good.Render("ok") + s.Muted.Render(" (certificate verified)")
	case p.HTTPSOK != nil:
		return s.Warn.Render("failed")
	}
	return s.Muted.Render("not checked (F: full scan)")
}

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}
