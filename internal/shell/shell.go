// Package shell is SilkRouter's shell integration: session files, the
// export / unset code `px on` / `px off` hand to the shell function that
// evals it, the zsh / bash / fish init scripts (wrapper functions,
// preexec / precmd logging hooks, powerlevel10k segment, plain prompt
// prefix), rc-file install blocks and the bootstrap files `px shell` uses
// to start a proxied subshell with the integration loaded.
//
// A child process cannot change its parent's environment, so the binary
// never edits the shell: it prints code, and the wrapper function defined
// by `eval "$(px shell-init zsh)"` evals it.
package shell

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Shells are the supported shells.
var Shells = []string{"zsh", "bash", "fish"}

// Supported reports whether sh is a supported shell name.
func Supported(sh string) bool {
	for _, s := range Shells {
		if s == sh {
			return true
		}
	}
	return false
}

// Detect guesses the user's shell from $SHELL ("zsh" by default).
func Detect() string {
	base := filepath.Base(os.Getenv("SHELL"))
	if Supported(base) {
		return base
	}
	return "zsh"
}

// Environment variables the integration exports.
const (
	EnvActive     = "SILKROUTER_ACTIVE"      // "1" while a proxy is on
	EnvProxy      = "SILKROUTER_PROXY"       // proxy URL (with credentials)
	EnvProxyKey   = "SILKROUTER_PROXY_KEY"   // scheme://host:port
	EnvProxyID    = "SILKROUTER_PROXY_ID"    // database id
	EnvProxyMS    = "SILKROUTER_PROXY_MS"    // latency when picked
	EnvProxyCC    = "SILKROUTER_PROXY_CC"    // exit country
	EnvSession    = "SILKROUTER_SESSION"     // session id
	EnvSince      = "SILKROUTER_SINCE"       // unix seconds
	EnvPrompt     = "SILKROUTER_PROMPT"      // segment text with the icon
	EnvPromptText = "SILKROUTER_PROMPT_TEXT" // segment text without the icon
	EnvIcon       = "SILKROUTER_ICON"
	EnvColor      = "SILKROUTER_COLOR"
	EnvLog        = "SILKROUTER_LOG"      // "1": hooks log commands
	EnvMethod     = "SILKROUTER_METHOD"   // env | proxychains
	EnvShellFn    = "SILKROUTER_SHELL_FN" // set by the wrapper function
	EnvSubshell   = "SILKROUTER_SUBSHELL" // "1" inside px shell
	savedPrefix   = "_SILKROUTER_SAVED_"
)

// ProxyVars are the standard variables set while a proxy is on.
var ProxyVars = []string{"ALL_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "all_proxy", "http_proxy", "https_proxy"}

// NoProxyVars carry the bypass list.
var NoProxyVars = []string{"NO_PROXY", "no_proxy"}

// sessionVars are every SILKROUTER_* variable `px off` removes.
var sessionVars = []string{EnvActive, EnvProxy, EnvProxyKey, EnvProxyID, EnvProxyMS, EnvProxyCC, EnvSession,
	EnvSince, EnvPrompt, EnvPromptText, EnvIcon, EnvColor, EnvLog, EnvMethod}

// Session is one `px on` (or `px shell`) session, stored as
// <home>/run/sessions/<id>.json (0600: the URL may carry credentials).
type Session struct {
	ID        string    `json:"id"`
	Proxy     string    `json:"proxy"` // URL with credentials
	Key       string    `json:"key"`   // scheme://host:port
	ProxyID   int64     `json:"proxy_id,omitempty"`
	MS        int       `json:"ms,omitempty"`
	Country   string    `json:"country,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitzero"`
	Rotations int       `json:"rotations,omitempty"`
	Shell     string    `json:"shell,omitempty"`
	PID       int       `json:"pid,omitempty"` // the shell
	TTY       string    `json:"tty,omitempty"`
	Kind      string    `json:"kind"` // on | subshell
	Method    string    `json:"method,omitempty"`
	// Select holds the selection options (JSON), reused by `px rotate`.
	Select json.RawMessage `json:"select,omitempty"`
}

// Active reports whether the session has not ended (and, on Linux, its
// shell process still exists).
func (s *Session) Active() bool {
	if !s.Ended.IsZero() {
		return false
	}
	if runtime.GOOS == "linux" && s.PID > 0 {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", s.PID)); err != nil {
			return false
		}
	}
	return true
}

// NewID returns a short random session id.
func NewID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// Store keeps session files in Dir (<home>/run/sessions).
type Store struct{ Dir string }

func (st Store) path(id string) string { return filepath.Join(st.Dir, id+".json") }

// Save writes s atomically.
func (st Store) Save(s *Session) error {
	if s.ID == "" || strings.ContainsAny(s.ID, `/\.`) {
		return fmt.Errorf("shell: bad session id %q", s.ID)
	}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(st.Dir, ".session-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	_ = os.Chmod(tmp.Name(), 0o600)
	return os.Rename(tmp.Name(), st.path(s.ID))
}

// Load reads one session.
func (st Store) Load(id string) (*Session, error) {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return nil, fmt.Errorf("shell: bad session id %q", id)
	}
	data, err := os.ReadFile(st.path(id))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	return &s, nil
}

// List returns every session, newest first.
func (st Store) List() ([]*Session, error) {
	ents, err := os.ReadDir(st.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		if s, err := st.Load(strings.TrimSuffix(name, ".json")); err == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, nil
}

// End marks a session ended (now).
func (st Store) End(id string) (*Session, error) {
	s, err := st.Load(id)
	if err != nil {
		return nil, err
	}
	if s.Ended.IsZero() {
		s.Ended = time.Now().UTC()
	}
	return s, st.Save(s)
}

// Prune deletes ended sessions that ended before t; it returns how many.
func (st Store) Prune(before time.Time) (int, error) {
	all, err := st.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range all {
		if !s.Active() && (s.Ended.IsZero() || s.Ended.Before(before)) && s.Started.Before(before) {
			if err := os.Remove(st.path(s.ID)); err == nil {
				n++
			}
		}
	}
	return n, nil
}

// ---- prompt text ------------------------------------------------------------

// PromptFields fill a prompt format.
type PromptFields struct {
	Icon, Scheme, Host, Country string
	Port, MS                    int
	ID                          int64
	Tags                        []string
}

// RenderPrompt expands {icon} {scheme} {host} {port} {ms} {cc} {id} {tags}
// and collapses the whitespace left by empty fields.
func RenderPrompt(format string, f PromptFields) string {
	ms := ""
	if f.MS > 0 {
		ms = strconv.Itoa(f.MS) + "ms"
	}
	id := ""
	if f.ID > 0 {
		id = "#" + strconv.FormatInt(f.ID, 10)
	}
	port := ""
	if f.Port > 0 {
		port = strconv.Itoa(f.Port)
	}
	r := strings.NewReplacer("{icon}", f.Icon, "{scheme}", f.Scheme, "{host}", f.Host, "{port}", port,
		"{ms}", ms, "{cc}", f.Country, "{id}", id, "{tags}", strings.Join(f.Tags, ","))
	out := strings.Join(strings.Fields(r.Replace(format)), " ")
	return strings.TrimSuffix(strings.TrimPrefix(out, ":"), ":")
}

// ---- emitted shell code ---------------------------------------------------------

// Var is one variable to export (Unset = remove it).
type Var struct {
	Name, Value string
	Unset       bool
}

// QuoteSh quotes s for sh / bash / zsh (single quotes).
func QuoteSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteFish quotes s for fish.
func QuoteFish(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// Emit renders vars as code for sh (zsh, bash) or fish.
func Emit(sh string, vars []Var) string {
	var b strings.Builder
	for _, v := range vars {
		switch {
		case sh == "fish" && v.Unset:
			fmt.Fprintf(&b, "set -e %s\n", v.Name)
		case sh == "fish":
			fmt.Fprintf(&b, "set -gx %s %s\n", v.Name, QuoteFish(v.Value))
		case v.Unset:
			fmt.Fprintf(&b, "unset %s\n", v.Name)
		default:
			fmt.Fprintf(&b, "export %s=%s\n", v.Name, QuoteSh(v.Value))
		}
	}
	return b.String()
}

// OnVars returns what `px on` exports: the previous proxy variables are
// saved once (restored by OffVars), then the proxy and session variables
// are set. lookup reads the shell's current environment.
func OnVars(s *Session, noProxy, icon, color, promptFormat string, logCommands bool, scheme, host string, port int,
	lookup func(string) (string, bool)) []Var {
	var out []Var
	if _, active := lookup(EnvActive); !active {
		for _, k := range append(append([]string{}, ProxyVars...), NoProxyVars...) {
			if v, ok := lookup(k); ok {
				out = append(out, Var{Name: savedPrefix + k, Value: v})
			}
		}
	}
	for _, k := range ProxyVars {
		out = append(out, Var{Name: k, Value: s.Proxy})
	}
	if noProxy != "" {
		for _, k := range NoProxyVars {
			out = append(out, Var{Name: k, Value: noProxy})
		}
	}
	f := PromptFields{Icon: icon, Scheme: scheme, Host: host, Port: port, MS: s.MS, Country: s.Country, ID: s.ProxyID, Tags: s.Tags}
	prompt := RenderPrompt(promptFormat, f)
	f.Icon = ""
	text := RenderPrompt(promptFormat, f)
	logv := "0"
	if logCommands {
		logv = "1"
	}
	method := s.Method
	if method == "" {
		method = "env"
	}
	out = append(out,
		Var{Name: EnvActive, Value: "1"},
		Var{Name: EnvProxy, Value: s.Proxy},
		Var{Name: EnvProxyKey, Value: s.Key},
		Var{Name: EnvProxyID, Value: strconv.FormatInt(s.ProxyID, 10)},
		Var{Name: EnvProxyMS, Value: strconv.Itoa(s.MS)},
		Var{Name: EnvProxyCC, Value: s.Country},
		Var{Name: EnvSession, Value: s.ID},
		Var{Name: EnvSince, Value: strconv.FormatInt(s.Started.Unix(), 10)},
		Var{Name: EnvPrompt, Value: prompt},
		Var{Name: EnvPromptText, Value: text},
		Var{Name: EnvIcon, Value: icon},
		Var{Name: EnvColor, Value: color},
		Var{Name: EnvLog, Value: logv},
		Var{Name: EnvMethod, Value: method},
	)
	return out
}

// OffVars restores the saved proxy variables (or removes them) and
// removes every session variable.
func OffVars(lookup func(string) (string, bool)) []Var {
	var out []Var
	for _, k := range append(append([]string{}, ProxyVars...), NoProxyVars...) {
		if v, ok := lookup(savedPrefix + k); ok {
			out = append(out, Var{Name: k, Value: v}, Var{Name: savedPrefix + k, Unset: true})
		} else {
			out = append(out, Var{Name: k, Unset: true})
		}
	}
	for _, k := range sessionVars {
		out = append(out, Var{Name: k, Unset: true})
	}
	return out
}

// EnvList renders vars as KEY=VALUE entries for a child process (unset
// ones are dropped from base).
func EnvList(base []string, vars []Var) []string {
	drop := map[string]bool{}
	for _, v := range vars {
		drop[v.Name] = true
	}
	out := make([]string, 0, len(base)+len(vars))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	for _, v := range vars {
		if !v.Unset {
			out = append(out, v.Name+"="+v.Value)
		}
	}
	return out
}
