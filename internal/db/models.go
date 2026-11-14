package db

import (
	"fmt"
	"strings"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/proxy"
)

// Proxy statuses.
const (
	StatusUnknown = "unknown"
	StatusAlive   = "alive"
	StatusDead    = "dead"
)

// Statuses lists every proxy status.
var Statuses = []string{StatusUnknown, StatusAlive, StatusDead}

// Measurement kinds.
const (
	KindProbe     = "probe"     // build / recache / import of a legacy cache
	KindPreflight = "preflight" // route checked the proxy before running
	KindRoute     = "route"     // a measured connection while routing
	KindScan      = "scan"      // px scan
)

// Kinds lists every measurement kind.
var Kinds = []string{KindProbe, KindPreflight, KindRoute, KindScan}

// ProbeLegacy is the probe name recorded for latencies imported from a
// legacy text cache.
const ProbeLegacy = "legacy"

// Error classes (measurements.err_class, usage_log.err_class).
const (
	ErrTimeout     = "timeout"
	ErrRefused     = "refused"
	ErrReset       = "reset"
	ErrDNS         = "dns"
	ErrTLS         = "tls"
	ErrAuth        = "auth"
	ErrProto       = "proto"
	ErrHTTPStatus  = "http_status"
	ErrExitNonzero = "exit_nonzero"
	ErrOther       = "other"
)

// ErrClasses lists the err_class vocabulary.
var ErrClasses = []string{ErrTimeout, ErrRefused, ErrReset, ErrDNS, ErrTLS, ErrAuth,
	ErrProto, ErrHTTPStatus, ErrExitNonzero, ErrOther}

// Reputation list kinds.
var ListKinds = []string{"dnsbl", "ipset", "proxylist", "torexit", "datacenter", "api"}

// Usage modes.
const (
	ModeRoute = "route"
	ModeShell = "shell"
	ModeRelay = "relay"
)

// Scan run states.
const (
	ScanRunning = "running"
	ScanPaused  = "paused"
	ScanDone    = "done"
	ScanFailed  = "failed"
)

// Proxy is one row of the proxies table: identity plus summary columns.
// Zero values mean "unknown" (NULL in the database).
type Proxy struct {
	ID               int64     `json:"id"`
	Scheme           string    `json:"scheme"`
	Host             string    `json:"host"`
	Port             int       `json:"port"`
	Username         string    `json:"username,omitempty"`
	Password         string    `json:"password,omitempty"`
	Source           string    `json:"source"`
	AddedAt          time.Time `json:"added_at"`
	LastSeenInSource time.Time `json:"last_seen_in_source,omitzero"`

	Status      string    `json:"status"`
	CheckedAt   time.Time `json:"checked_at,omitzero"`
	LastMS      int       `json:"last_ms,omitempty"`
	BestMS      int       `json:"best_ms,omitempty"`
	AvgMS       float64   `json:"avg_ms,omitempty"`
	MSCount     int       `json:"ms_count"`
	MSSum       int64     `json:"ms_sum"`
	OKCount     int       `json:"ok_count"`
	FailCount   int       `json:"fail_count"`
	OKStreak    int       `json:"ok_streak"`
	FailStreak  int       `json:"fail_streak"`
	ExitIP      string    `json:"exit_ip,omitempty"`
	Country     string    `json:"country,omitempty"`
	ASN         string    `json:"asn,omitempty"`
	Org         string    `json:"org,omitempty"`
	Anonymity   string    `json:"anonymity"`
	ListedCount int       `json:"listed_count"`
	SpeedScore  *float64  `json:"speed_score,omitempty"`
	CleanScore  *float64  `json:"clean_score,omitempty"`
	Rank        *int      `json:"rank,omitempty"`

	// Learned by full scans (schema v2).
	Protocols   []string  `json:"protocols,omitempty"` // schemes verified on host:port
	HTTPSOK     *bool     `json:"https_ok,omitempty"`  // TLS through the proxy worked
	JitterMS    *float64  `json:"jitter_ms,omitempty"`
	Region      string    `json:"region,omitempty"`
	City        string    `json:"city,omitempty"`
	ISP         string    `json:"isp,omitempty"`
	Hosting     *bool     `json:"hosting,omitempty"` // datacenter / hosting range
	Mobile      *bool     `json:"mobile,omitempty"`
	FullScanAt  time.Time `json:"full_scan_at,omitzero"`
	StableScore *float64  `json:"stable_score,omitempty"`

	// Usage counters (from usage_log).
	UseCount    int       `json:"use_count"`
	UseOK       int       `json:"use_ok"`
	UseFail     int       `json:"use_fail"`
	UseProxyErr int       `json:"use_proxy_err"`
	LastUsedAt  time.Time `json:"last_used_at,omitzero"`
	LastCmd     string    `json:"last_cmd,omitempty"`

	Tags []string `json:"tags,omitempty"` // filled when Filter.WithTags
}

// SuccessRate is ok / (ok + fail) over every fact, or -1 when unmeasured.
func (p *Proxy) SuccessRate() float64 {
	n := p.OKCount + p.FailCount
	if n == 0 {
		return -1
	}
	return float64(p.OKCount) / float64(n)
}

// HasTag reports whether p carries label ("tag" or "tag:scope"); Tags must
// have been loaded (Filter.WithTags).
func (p *Proxy) HasTag(label string) bool {
	for _, t := range p.Tags {
		if t == label {
			return true
		}
	}
	return false
}

// ProxyErrClasses are the err_class values that point at the proxy or the
// network rather than at the routed program itself.
var ProxyErrClasses = []string{ErrTimeout, ErrRefused, ErrReset, ErrDNS, ErrTLS, ErrAuth, ErrProto}

// IsProxyErr reports whether class is one of ProxyErrClasses.
func IsProxyErr(class string) bool {
	for _, c := range ProxyErrClasses {
		if c == class {
			return true
		}
	}
	return false
}

// ProxyInfo is what a scan learned about a proxy beyond measurements. Empty
// strings, nil pointers, a nil Protocols slice and a zero FullScanAt leave
// the stored value unchanged.
type ProxyInfo struct {
	ProxyID    int64
	Addr       Addr // used when ProxyID is 0
	Country    string
	Region     string
	City       string
	ASN        string
	Org        string
	ISP        string
	Anonymity  string
	Hosting    *bool
	Mobile     *bool
	HTTPSOK    *bool
	JitterMS   *float64
	Protocols  []string
	FullScanAt time.Time
}

// Bool returns a pointer to b (for ProxyInfo fields).
func Bool(b bool) *bool { return &b }

// Float returns a pointer to f.
func Float(f float64) *float64 { return &f }

// URL is scheme://[user:pass@]host:port.
func (p *Proxy) URL() string { return p.ToProxy().URL() }

// Key is scheme://host:port (no credentials), the identity used everywhere.
func (p *Proxy) Key() string { return p.ToProxy().Key() }

// ToProxy converts to the dialer model; LatencyMS is last_ms.
func (p *Proxy) ToProxy() proxy.Proxy {
	return proxy.Proxy{
		Scheme: proxy.Scheme(p.Scheme), Host: p.Host, Port: p.Port,
		User: p.Username, Pass: p.Password, LatencyMS: p.LastMS,
	}
}

// Addr identifies a proxy by scheme, host and port (plus optional
// credentials) when its database id is not known yet.
type Addr struct {
	Scheme   string
	Host     string
	Port     int
	Username string
	Password string
}

// AddrOf converts a parsed proxy.
func AddrOf(p proxy.Proxy) Addr {
	return Addr{Scheme: string(p.Scheme), Host: p.Host, Port: p.Port, Username: p.User, Password: p.Pass}
}

// ParseAddr parses anything proxy.Parse accepts.
func ParseAddr(s string) (Addr, error) {
	p, err := proxy.Parse(strings.TrimSpace(s))
	if err != nil {
		return Addr{}, err
	}
	return AddrOf(p), nil
}

func (a Addr) String() string { return fmt.Sprintf("%s://%s:%d", a.Scheme, a.Host, a.Port) }

// Measurement is one observation of a proxy (a fact row).
type Measurement struct {
	ID int64 `json:"id"`
	// ProxyID is the proxy; when 0, Addr is used to find or create it.
	ProxyID  int64     `json:"proxy_id"`
	Addr     Addr      `json:"-"`
	TS       time.Time `json:"ts"` // zero = now
	Kind     string    `json:"kind"`
	Probe    string    `json:"probe,omitempty"`
	OK       bool      `json:"ok"`
	MS       int       `json:"ms,omitempty"` // 0 = not timed
	ErrClass string    `json:"err_class,omitempty"`
	ExitIP   string    `json:"exit_ip,omitempty"`
	Detail   string    `json:"detail,omitempty"` // JSON text
}

// Reputation is one (ip, list) verdict.
type Reputation struct {
	ID        int64     `json:"id"`
	IP        string    `json:"ip"`
	ListName  string    `json:"list_name"`
	ListKind  string    `json:"list_kind"`
	Listed    bool      `json:"listed"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"` // zero = now
}

// Usage is one usage_log row (a proxied command).
type Usage struct {
	ID          int64     `json:"id"`
	TSStart     time.Time `json:"ts_start"` // zero = now
	TSEnd       time.Time `json:"ts_end,omitzero"`
	ProxyID     int64     `json:"proxy_id,omitempty"` // 0 = NULL
	ProxyURL    string    `json:"proxy_url"`
	Mode        string    `json:"mode"`
	Argv        []string  `json:"argv"`
	Cmd         string    `json:"cmd"`
	Cwd         string    `json:"cwd,omitempty"`
	TTY         string    `json:"tty,omitempty"`
	PID         int       `json:"pid,omitempty"`
	PPID        int       `json:"ppid,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	PreflightOK *bool     `json:"preflight_ok,omitempty"`
	PreflightMS *int      `json:"preflight_ms,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
	ErrClass    string    `json:"err_class,omitempty"`
	MS          int       `json:"ms,omitempty"` // proxy latency measured during the command
}

// OK reports whether the command finished with exit code 0.
func (u *Usage) OK() bool { return u.ExitCode != nil && *u.ExitCode == 0 }

// Tag is one heuristic or manual tag on a proxy.
type Tag struct {
	ProxyID int64     `json:"proxy_id"`
	Tag     string    `json:"tag"`
	Scope   string    `json:"scope,omitempty"`
	Since   time.Time `json:"since"`
	Reason  string    `json:"reason,omitempty"`
}

// Label renders the tag as shown to users: "tag" or "tag:scope".
func (t Tag) Label() string {
	if t.Scope == "" {
		return t.Tag
	}
	return t.Tag + ":" + t.Scope
}

// ScanRun is one (resumable) scan.
type ScanRun struct {
	ID         int64     `json:"id"`
	Mode       string    `json:"mode"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Total      int       `json:"total"`
	Done       int       `json:"done"`
	Cursor     string    `json:"cursor,omitempty"`
	Opts       string    `json:"opts,omitempty"` // JSON text
	State      string    `json:"state"`
}
