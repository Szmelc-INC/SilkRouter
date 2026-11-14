package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/szmelc-inc/silkrouter/internal/config"
	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/paths"
	"github.com/szmelc-inc/silkrouter/internal/probe"
	"github.com/szmelc-inc/silkrouter/internal/reputation"
	"github.com/szmelc-inc/silkrouter/internal/score"
)

// ---- result messages ------------------------------------------------------------

type checkDoneMsg struct {
	job   int
	id    int64
	key   string
	probe string
	res   probe.Result
	proxy *db.Proxy // reloaded row (nil on error)
	err   error
}

func (m checkDoneMsg) jobID() int { return m.job }

type tagsChangedMsg struct {
	id   int64
	text string
	warn string
	err  error
}

type rankDoneMsg struct {
	job  int
	mode string
	res  score.Result
	tags score.TagResult
	err  error
}

func (m rankDoneMsg) jobID() int { return m.job }

type retagDoneMsg struct {
	job int
	res score.TagResult
	err error
}

func (m retagDoneMsg) jobID() int { return m.job }

type listsDoneMsg struct {
	job        int
	ok, failed int
	firstErr   error
}

func (m listsDoneMsg) jobID() int { return m.job }

type exportDoneMsg struct {
	path string
	n    int
	err  error
}

type configSavedMsg struct {
	key   string
	value string
	reset bool
	eff   *config.Config // effective config after the write
	err   error
}

// handleResult turns job / action results into toasts and applies saved
// configuration (views update their own data from the same messages).
func (m *Model) handleResult(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case checkDoneMsg:
		switch {
		case errors.Is(msg.err, context.Canceled):
			return nil
		case msg.err != nil:
			return m.notify(levelErr, "check %s: %v", msg.key, msg.err)
		case msg.res.OK:
			extra := ""
			if msg.res.ExitIP != "" {
				extra = " · exit " + msg.res.ExitIP
			}
			return m.notify(levelOK, "✓ %s alive %s (%s)%s", msg.key, msUnit(msg.res.LatencyMS), msg.probe, extra)
		default:
			class := probe.ClassifyErr(msg.res.Err)
			return m.notify(levelWarn, "✗ %s failed (%s): %v", msg.key, dash(class), msg.res.Err)
		}
	case tagsChangedMsg:
		m.tagLabels = nil
		if msg.err != nil {
			return m.notify(levelErr, "%v", msg.err)
		}
		if msg.warn != "" {
			return m.notify(levelWarn, "%s (%s)", msg.text, msg.warn)
		}
		return m.notify(levelOK, "%s", msg.text)
	case rankDoneMsg:
		if msg.err != nil {
			return m.notify(levelErr, "rank: %v", msg.err)
		}
		m.tagLabels = nil
		return m.notify(levelOK, "scored %d proxies, ranked %d alive (%s) · tags: %s", msg.res.Scored, msg.res.Ranked,
			msg.mode, countsText(msg.tags.Counts))
	case retagDoneMsg:
		if msg.err != nil {
			return m.notify(levelErr, "tags: %v", msg.err)
		}
		m.tagLabels = nil
		return m.notify(levelOK, "set %d heuristic tags: %s", msg.res.Total, countsText(msg.res.Counts))
	case listsDoneMsg:
		if msg.failed > 0 {
			return m.notify(levelWarn, "lists: %d updated, %d failed (%v)", msg.ok, msg.failed, msg.firstErr)
		}
		return m.notify(levelOK, "lists: %d up to date", msg.ok)
	case exportDoneMsg:
		if msg.err != nil {
			return m.notify(levelErr, "export: %v", msg.err)
		}
		return m.notify(levelOK, "exported %d proxy URLs to %s", msg.n, paths.Pretty(msg.path))
	case configSavedMsg:
		if msg.err != nil {
			return m.notify(levelErr, "%v", msg.err)
		}
		m.cfg = msg.eff
		what := fmt.Sprintf("%s = %s (%s)", msg.key, showValue(msg.value), paths.Pretty(m.opts.ConfigPath))
		if msg.reset {
			what = fmt.Sprintf("%s = %s (default)", msg.key, showValue(msg.value))
		}
		var note tea.Cmd
		if src := m.cfg.Origin(msg.key); strings.HasPrefix(src, "env ") {
			note = m.notify(levelWarn, "%s; note: $%s overrides it here", what, strings.TrimPrefix(src, "env "))
		} else {
			note = m.notify(levelOK, "%s", what)
		}
		// Applied last, so a warning from applying it wins the toast.
		return tea.Batch(note, m.applyConfigKey(msg.key))
	}
	return nil
}

func countsText(c map[string]int) string {
	if len(c) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, c[k])
	}
	return strings.Join(parts, ", ")
}

// ---- callbacks with built-in fallbacks ----------------------------------------

func (m *Model) scoreOptions(mode string) score.Options {
	if m.opts.ScoreOptions != nil {
		return m.opts.ScoreOptions(m.cfg, mode)
	}
	c := m.cfg.Scan
	return score.Options{Mode: mode, WeightSpeed: c.WeightSpeed, WeightClean: c.WeightClean, WeightStable: c.WeightStable,
		SpeedRefMS: c.SpeedRefMS}
}

func (m *Model) tagOptions() score.TagOptions {
	if m.opts.TagOptions != nil {
		return m.opts.TagOptions(m.cfg)
	}
	t := m.cfg.Tags
	return score.TagOptions{StableN: t.StableN, FastN: t.FastN, FastMS: t.FastMS, FavMinUses: t.FavMinUses,
		FavMinRate: t.FavMinRate, RecommendTop: t.RecommendTop, FlakyMinFacts: t.FlakyMinFacts,
		FlakyMaxRate: t.FlakyMaxRate, Window: t.Window.D()}
}

func (m *Model) listManager() (*reputation.Manager, error) {
	if m.opts.ListManager != nil {
		return m.opts.ListManager(m.cfg)
	}
	ls, err := reputation.Select(reputation.Builtin, m.cfg.Scan.ListNames)
	if err != nil {
		return nil, err
	}
	return &reputation.Manager{Dir: paths.ListsDir(), TTL: m.cfg.Scan.ListsTTL.D(), Lists: ls}, nil
}

// probe is the probe of on-demand checks: probe.test_url, else probe.name.
func (m *Model) probe() (probe.Probe, error) {
	if u := m.cfg.Probe.TestURL; u != "" {
		return probe.Custom(u), nil
	}
	return probe.Get(m.cfg.Probe.Name)
}

// ---- actions ----------------------------------------------------------------------

// checkProxy measures p now in the background (check.tries, check.timeout)
// and records the result as a probe measurement.
func (m *Model) checkProxy(p *db.Proxy) tea.Cmd {
	if p == nil {
		return m.notify(levelWarn, "no proxy selected")
	}
	pr, err := m.probe()
	if err != nil {
		return m.notify(levelErr, "check: %v", err)
	}
	job, spin := m.startJob("check #" + strconv.FormatInt(p.ID, 10))
	ctx, d := m.ctx, m.d
	px, id, label := p.ToProxy(), p.ID, p.Key()
	tries := max(1, m.cfg.Check.Tries)
	timeout := m.cfg.Check.Timeout.D()
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	run := func() tea.Msg {
		res := pr.Measure(ctx, px, tries, timeout)
		out := checkDoneMsg{job: job, id: id, key: label, probe: pr.Name, res: res}
		if ctx.Err() != nil {
			out.err = ctx.Err() // quitting: do not record a cancelled check as a failure
			return out
		}
		_, out.err = d.RecordMeasurement(ctx, db.Measurement{ProxyID: id, Kind: db.KindProbe, Probe: pr.Name, OK: res.OK,
			MS: res.LatencyMS, ErrClass: probe.ClassifyErr(res.Err), ExitIP: res.ExitIP})
		if out.err == nil {
			if np, err := d.GetProxy(ctx, id); err == nil {
				if ts, err := d.Tags(ctx, id); err == nil {
					for _, t := range ts {
						np.Tags = append(np.Tags, t.Label())
					}
				}
				out.proxy = np
			}
		}
		return out
	}
	return tea.Batch(spin, m.notify(levelInfo, "checking %s with %s…", label, pr.Name), run)
}

// addTags sets manual tags ("tag" or "tag:scope") on a proxy.
func (m *Model) addTags(id int64, label string, tags []string) tea.Cmd {
	d, ctx := m.d, m.ctx
	return func() tea.Msg {
		var ts []db.Tag
		var heur, names []string
		for _, l := range tags {
			tag, scope, _ := strings.Cut(strings.TrimSpace(l), ":")
			if tag == "" {
				continue
			}
			if score.IsHeuristic(tag) {
				heur = append(heur, tag)
			}
			names = append(names, l)
			ts = append(ts, db.Tag{ProxyID: id, Tag: tag, Scope: scope, Reason: "tui"})
		}
		if len(ts) == 0 {
			return toastMsg{levelWarn, "no tag given"}
		}
		if err := d.SetTags(ctx, ts); err != nil {
			return tagsChangedMsg{id: id, err: err}
		}
		out := tagsChangedMsg{id: id, text: fmt.Sprintf("tagged %s: %s", label, strings.Join(names, " "))}
		if len(heur) > 0 {
			out.warn = "heuristic " + strings.Join(heur, ", ") + ": the next refresh recomputes it"
		}
		return out
	}
}

// removeTags clears tags from a proxy.
func (m *Model) removeTags(id int64, label string, tags []string) tea.Cmd {
	d, ctx := m.d, m.ctx
	return func() tea.Msg {
		for _, l := range tags {
			tag, scope, _ := strings.Cut(strings.TrimSpace(l), ":")
			if err := d.ClearTag(ctx, id, tag, scope); err != nil {
				return tagsChangedMsg{id: id, err: err}
			}
		}
		return tagsChangedMsg{id: id, text: fmt.Sprintf("untagged %s: %s", label, strings.Join(tags, " "))}
	}
}

// setBlocked sets (on) or clears the manual "blocked" tag; toggle reads
// the current state first.
func (m *Model) setBlocked(p *db.Proxy, on *bool) tea.Cmd {
	if p == nil {
		return m.notify(levelWarn, "no proxy selected")
	}
	d, ctx := m.d, m.ctx
	id, label := p.ID, p.Key()
	return func() tea.Msg {
		block := true
		if on != nil {
			block = *on
		} else {
			ts, err := d.Tags(ctx, id)
			if err != nil {
				return tagsChangedMsg{id: id, err: err}
			}
			for _, t := range ts {
				if t.Tag == "blocked" && t.Scope == "" {
					block = false
				}
			}
		}
		if !block {
			if err := d.ClearTag(ctx, id, "blocked", ""); err != nil {
				return tagsChangedMsg{id: id, err: err}
			}
			return tagsChangedMsg{id: id, text: "unblocked " + label}
		}
		if err := d.SetTag(ctx, db.Tag{ProxyID: id, Tag: "blocked", Reason: "tui"}); err != nil {
			return tagsChangedMsg{id: id, err: err}
		}
		return tagsChangedMsg{id: id, text: "blocked " + label + " (route skips it)"}
	}
}

func (m *Model) toggleBlocked(p *db.Proxy) tea.Cmd { return m.setBlocked(p, nil) }

// copyURL puts the proxy URL on the clipboard with OSC 52 (works over SSH;
// tmux and screen get their passthrough wrappers).
func (m *Model) copyURL(p *db.Proxy) tea.Cmd {
	if p == nil {
		return m.notify(levelWarn, "no proxy selected")
	}
	w, env := m.opts.Clipboard, m.opts.Getenv
	url, label := p.URL(), p.Key()
	return func() tea.Msg {
		seq := osc52.New(url)
		switch {
		case env("TMUX") != "":
			seq = seq.Tmux()
		case strings.HasPrefix(env("TERM"), "screen"):
			seq = seq.Screen()
		}
		if _, err := seq.WriteTo(w); err != nil {
			return toastMsg{levelErr, "copy: " + err.Error()}
		}
		return toastMsg{levelOK, "copied " + label + " to the clipboard (OSC 52)"}
	}
}

// rank recomputes scores and ranks (px rank), then the heuristic tags.
func (m *Model) rank(mode string) tea.Cmd {
	if mode == "" {
		mode = m.cfg.Scan.Rank
	}
	if !contains(score.RankModes, mode) {
		return m.notify(levelErr, "unknown rank mode %q (want one of: %s)", mode, strings.Join(score.RankModes, ", "))
	}
	job, spin := m.startJob("rank (" + mode + ")")
	ctx, d := m.ctx, m.d
	so, to := m.scoreOptions(mode), m.tagOptions()
	return tea.Batch(spin, func() tea.Msg {
		res, err := score.Compute(ctx, d, so)
		if err != nil {
			return rankDoneMsg{job: job, mode: mode, err: err}
		}
		tr, err := score.RefreshTags(ctx, d, to)
		return rankDoneMsg{job: job, mode: mode, res: res, tags: tr, err: err}
	})
}

// retag recomputes every heuristic tag (px tags refresh).
func (m *Model) retag() tea.Cmd {
	job, spin := m.startJob("refresh tags")
	ctx, d, to := m.ctx, m.d, m.tagOptions()
	return tea.Batch(spin, func() tea.Msg {
		res, err := score.RefreshTags(ctx, d, to)
		return retagDoneMsg{job: job, res: res, err: err}
	})
}

// updateLists downloads stale (or all) reputation lists in the background.
func (m *Model) updateLists(force bool) tea.Cmd {
	mgr, err := m.listManager()
	if err != nil {
		return m.notify(levelErr, "lists: %v", err)
	}
	job, spin := m.startJob("lists update")
	ctx := m.ctx
	return tea.Batch(spin, m.notify(levelInfo, "downloading lists…"), func() tea.Msg {
		out := listsDoneMsg{job: job}
		for _, r := range mgr.Load(ctx, true, force) {
			if r.Err != nil && r.Set == nil {
				out.failed++
				if out.firstErr == nil {
					out.firstErr = r.Err
				}
				continue
			}
			out.ok++
		}
		return out
	})
}

// exportURLs writes the URLs of every proxy matching the proxy table's
// filter (all pages) to path, atomically.
func (m *Model) exportURLs(path string) tea.Cmd {
	if strings.TrimSpace(path) == "" {
		return m.notify(levelErr, "usage: :export FILE")
	}
	path = paths.ExpandUser(path)
	f := m.prox.f.dbFilter()
	f.Limit, f.Offset, f.WithTags = 0, 0, false
	d, ctx := m.d, m.ctx
	return func() tea.Msg {
		var buf bytes.Buffer
		n, err := d.Export(ctx, &buf, db.FormatURLs, f)
		if err == nil {
			err = config.WriteFileAtomic(path, buf.Bytes(), 0o600)
		}
		return exportDoneMsg{path: path, n: n, err: err}
	}
}

// setConfig writes key = value (or resets it) to config.toml exactly like
// 'px config set' / 'px config reset KEY': read the file without the
// environment layer, validate, back it up, save it in canonical form.
func (m *Model) setConfig(key, value string, reset bool) tea.Cmd {
	path := m.opts.ConfigPath
	if _, ok := config.Lookup(key); !ok {
		return m.notify(levelErr, "unknown config key %q", key)
	}
	return func() tea.Msg {
		out := configSavedMsg{key: key, reset: reset}
		if path == "" {
			out.err = errors.New("no config file")
			return out
		}
		fc, err := config.LoadFile(path)
		if err != nil {
			out.err = fmt.Errorf("%v (fix it with px config edit first)", err)
			return out
		}
		if reset {
			err = fc.Reset(key)
		} else {
			err = fc.Set(key, value)
		}
		if err != nil {
			out.err = err
			return out
		}
		if _, err := config.Backup(path); err != nil {
			out.err = fmt.Errorf("backup: %w", err)
			return out
		}
		if err := config.Save(path, fc); err != nil {
			out.err = err
			return out
		}
		out.value, _ = fc.Get(key)
		out.eff, _ = config.Load(path, os.LookupEnv)
		return out
	}
}

// applyConfigKey makes a saved [tui] key take effect immediately.
func (m *Model) applyConfigKey(key string) tea.Cmd {
	c := m.cfg.TUI
	switch key {
	case "tui.theme":
		if err := m.setTheme(c.Theme); err != nil {
			return m.notify(levelErr, "%v", err)
		}
	case "tui.refresh":
		m.refreshEvery = c.Refresh.D()
		return m.scheduleTick()
	case "tui.mouse":
		if c.Mouse {
			return tea.EnableMouseCellMotion
		}
		return tea.DisableMouse
	case "tui.alt_screen":
		if c.AltScreen {
			return tea.EnterAltScreen
		}
		return tea.ExitAltScreen
	case "tui.columns":
		if bad := m.prox.setColumns(m); len(bad) > 0 {
			return m.notify(levelWarn, "%s", columnsWarning(bad))
		}
	case "tui.page_size":
		return tea.Batch(m.prox.reloadFiltered(m), m.hist.restart(m))
	case "tui.show_dead":
		m.prox.f.showDead = c.ShowDeadRows
		return m.prox.reloadFiltered(m)
	case "tui.markdown":
		m.help.cacheKey = ""
	}
	return nil
}

// showProxy opens the details of proxy id in the Proxies tab.
func (m *Model) showProxy(id int64) tea.Cmd {
	m.active = indexOf(TabProxies)
	m.help.open = false
	return tea.Batch(m.prox.refresh(m), m.prox.openDetail(m, id))
}

// showTag switches to the Proxies tab filtered by a tag (every status).
func (m *Model) showTag(tag, scope string) tea.Cmd {
	f := m.prox.f.cleared()
	f.status, f.showDead, f.tag, f.tagScope = "all", true, tag, scope
	m.prox.f = f
	m.prox.detail = nil
	m.active = indexOf(TabProxies)
	label := tag
	if scope != "" {
		label += ":" + scope
	}
	return tea.Batch(m.prox.reloadFiltered(m), m.notify(levelInfo, "proxies tagged %s (X clears the filter)", label))
}

func indexOf(tab string) int {
	for i, t := range tabOrder {
		if t == tab {
			return i
		}
	}
	return 0
}

// loadTagLabels fills the tag-name cache used for completion.
func (m *Model) loadTagLabels() tea.Cmd {
	if m.tagLabels != nil {
		return nil
	}
	d, ctx := m.d, m.ctx
	return func() tea.Msg {
		counts, err := d.TagCounts(ctx)
		if err != nil {
			return nil
		}
		labels := make([]string, 0, len(counts))
		for _, t := range counts {
			labels = append(labels, labelOf(t))
		}
		return tagLabelsMsg(labels)
	}
}

type tagLabelsMsg []string
