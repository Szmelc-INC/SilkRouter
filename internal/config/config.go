// Package config holds SilkRouter's typed configuration (config.toml).
//
// Precedence: built-in defaults < config.toml < environment < flags. The
// first three are resolved here (Load); flags use the loaded values as their
// defaults, so whatever a flag parses is already the final value.
//
// Every key is a field of Config (or of a section struct) with struct tags:
//
//	toml:"max_ms"           key name (required)
//	help:"..."              one-line description (config file, `config keys`)
//	choices:"a|b|c"         allowed values (validated on load and set)
//	min:"1"                 minimum for numeric keys
//	env:"PX_LIST"           extra (legacy) environment names
//
// Each key is also readable from SILKROUTER_<SECTION>_<KEY> (dots become
// underscores, upper-cased), e.g. route.max_ms <- SILKROUTER_ROUTE_MAX_MS.
// Adding a key is one struct field plus its default in Default(); get/set,
// env overrides, validation, `config keys`, completion and the commented
// config writer all pick it up through reflection.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/szmelc-inc/silkrouter/internal/paths"
)

// Config is the whole configuration file. Sections that are still empty are
// declared so later features (and users' files) have a stable place to go.
type Config struct {
	Cache      CacheConfig      `toml:"cache" help:"Legacy text cache (scheme://host:port [Nms], fastest first)."`
	Source     SourceConfig     `toml:"source" help:"Where candidate proxies come from."`
	Probe      ProbeConfig      `toml:"probe" help:"How a proxy is validated and timed (see: px probes)."`
	Route      RouteConfig      `toml:"route" help:"Defaults for 'px route' (and legacy 'px <command...>')."`
	Build      BuildConfig      `toml:"build" help:"Defaults for 'px build' and 'px recache'."`
	Check      CheckConfig      `toml:"check" help:"Defaults for 'px check'."`
	BLCheck    BLCheckConfig    `toml:"blcheck" help:"Defaults for 'px blcheck' (mxtoolbox DNS blacklists)."`
	ProxyCheck ProxyCheckConfig `toml:"proxycheck" help:"Defaults for 'px proxycheck' (whatismyipaddress)."`
	UI         UIConfig         `toml:"ui" help:"Terminal output."`
	DB         DBConfig         `toml:"db" help:"SQLite database."`
	Scan       ScanConfig       `toml:"scan" help:"Full / resumable scans."`
	Tags       TagsConfig       `toml:"tags" help:"Heuristic tag thresholds (stable, fast, fav:<cmd>, ...)."`
	History    HistoryConfig    `toml:"history" help:"Usage log of proxied commands."`
	Shell      ShellConfig      `toml:"shell" help:"Shell integration (px on / px off, prompt segment)."`
	TUI        TUIConfig        `toml:"tui" help:"Interactive terminal UI."`

	meta meta
}

// CacheConfig configures the legacy text cache.
type CacheConfig struct {
	File   string `toml:"file" help:"text cache file; empty means <home>/cache.list" env:"PX_LIST"`
	Mirror bool   `toml:"mirror" help:"also rewrite <home>/cache.list on build/recache in database mode (px.sh compatibility)"`
}

// SourceConfig configures candidate lists.
type SourceConfig struct {
	URL     string   `toml:"url" help:"proxy-list URL; empty means the upstream SilkRouter list"`
	Builtin bool     `toml:"builtin" help:"use the list embedded in the binary instead of downloading"`
	Timeout Duration `toml:"timeout" help:"download timeout for the source list"`
}

// ProbeConfig configures validation probes.
type ProbeConfig struct {
	Name    string `toml:"name" help:"default probe (px probes lists them)"`
	TestURL string `toml:"test_url" help:"custom probe URL (any 2xx counts as alive); overrides name"`
}

// RouteConfig configures proxy selection for route.
type RouteConfig struct {
	MaxMS         int      `toml:"max_ms" help:"maximum cached latency in ms" min:"0"`
	MinMS         int      `toml:"min_ms" help:"minimum cached latency in ms" min:"0"`
	Scheme        string   `toml:"scheme" help:"protocol filter" choices:"any|http|socks"`
	Verify        string   `toml:"verify" help:"verify the pick first: auto = only for fresh proxies" choices:"auto|skip|always"`
	VerifyTimeout Duration `toml:"verify_timeout" help:"timeout of one verification"`
	AutoBuild     bool     `toml:"auto_build" help:"build the cache without asking when it is missing"`
	Pool          string   `toml:"pool" help:"where route picks proxies: the database, or the legacy text cache" choices:"db|cache"`
	Prefer        string   `toml:"prefer" help:"pick order: smart = recommended:/fav:<cmd> first then random, rank = best ranked first, random" choices:"smart|rank|random"`
	ExcludeTags   []string `toml:"exclude_tags" help:"never route through proxies carrying one of these tags"`
	Method        string   `toml:"method" help:"how the command is proxied: env = proxy variables, proxychains = proxychains4 (every TCP connection of the process tree)" choices:"env|proxychains"`
	Proxychains   string   `toml:"proxychains" help:"proxychains binary (empty = proxychains4, then proxychains)"`
}

// BuildConfig configures cache builds.
type BuildConfig struct {
	Workers  int      `toml:"workers" help:"parallel workers" min:"1"`
	Tries    int      `toml:"tries" help:"samples averaged per proxy" min:"1"`
	Timeout  Duration `toml:"timeout" help:"per-try timeout"`
	ShowDead bool     `toml:"show_dead" help:"also log dead proxies while building"`
}

// CheckConfig configures the check command.
type CheckConfig struct {
	Workers int      `toml:"workers" help:"parallel workers (file mode)" min:"1"`
	Tries   int      `toml:"tries" help:"samples averaged per proxy" min:"1"`
	Timeout Duration `toml:"timeout" help:"per-try timeout"`
}

// BLCheckConfig configures the blacklist checker.
type BLCheckConfig struct {
	Token   string   `toml:"token" help:"mxtoolbox temp auth token; empty means fetch one" env:"MXTB_TOKEN"`
	Delay   Duration `toml:"delay" help:"delay between lookups in file mode"`
	Timeout Duration `toml:"timeout" help:"HTTP timeout per lookup"`
}

// ProxyCheckConfig configures the anonymity checker.
type ProxyCheckConfig struct {
	Timeout Duration `toml:"timeout" help:"connect / request timeout"`
	Delay   Duration `toml:"delay" help:"delay between requests in file mode"`
}

// UIConfig configures terminal output.
type UIConfig struct {
	Color string `toml:"color" help:"colored output (NO_COLOR and --no-color force never)" choices:"auto|always|never"`
}

// DBConfig configures the SQLite database.
type DBConfig struct {
	Path          string   `toml:"path" help:"database file; empty means <home>/silkrouter.db"`
	AutoImport    bool     `toml:"auto_import" help:"fill an empty database from the embedded list and cache.list"`
	BatchSize     int      `toml:"batch_size" help:"rows per write transaction for bulk writers (build, scans)" min:"1"`
	FlushInterval Duration `toml:"flush_interval" help:"commit pending bulk rows at least this often"`
	BusyTimeout   Duration `toml:"busy_timeout" help:"how long to wait for a lock held by another process"`
	Record        bool     `toml:"record" help:"also record results in the database when a legacy text cache is used, and record route verifications"`
	Backup        bool     `toml:"backup" help:"back the database up before 'px db reset'"`
	ListLimit     int      `toml:"list_limit" help:"default row limit of 'px proxies' (0 = all)" min:"0"`
	ShowRecent    int      `toml:"show_recent" help:"recent measurements shown by 'px show'" min:"0"`
}

// ScanConfig configures 'px scan' (quick / full / reputation / geo / protocols).
type ScanConfig struct {
	Workers      int      `toml:"workers" help:"proxies scanned in parallel (full scans stay gentle)" min:"1"`
	QuickWorkers int      `toml:"quick_workers" help:"proxies tested in parallel by 'scan quick'" min:"1"`
	Tries        int      `toml:"tries" help:"samples per probe in a full scan (latency and jitter)" min:"1"`
	Timeout      Duration `toml:"timeout" help:"timeout of one check"`
	Delay        Duration `toml:"delay" help:"pause between two checks of the same proxy (gentle mode)"`
	Probes       []string `toml:"probes" help:"connectivity probes a full scan runs on every proxy (px probes)"`
	FailFast     int      `toml:"fail_fast" help:"stop checking a proxy after this many probes failed completely (0 = never)" min:"0"`
	ExitProbe    string   `toml:"exit_probe" help:"probe that reveals the exit IP"`
	HTTPSURL     string   `toml:"https_url" help:"HTTPS URL fetched through the proxy (TLS / CONNECT check; Cloudflare trace also yields the exit country)"`
	JudgeURL     string   `toml:"judge_url" help:"header-echo URL that grades anonymity (transparent / anonymous / elite)"`
	RealIPURL    string   `toml:"real_ip_url" help:"URL fetched directly (no proxy) once per scan to learn your real IP"`
	Protocols    bool     `toml:"protocols" help:"also detect which protocols each address speaks (http, socks4, socks5)"`
	DNSBL        bool     `toml:"dnsbl" help:"look exit IPs up on DNS blacklists"`
	DNSBLZones   []string `toml:"dnsbl_zones" help:"DNS blacklist zones queried"`
	DNSTimeout   Duration `toml:"dns_timeout" help:"timeout of one DNS blacklist lookup"`
	Lists        bool     `toml:"lists" help:"match IPs against downloaded known-proxy / tor / abuse / datacenter lists"`
	ListNames    []string `toml:"list_names" help:"lists used by scans (px lists shows all; empty = every built-in list)"`
	ListsTTL     Duration `toml:"lists_ttl" help:"re-download a list when the local copy is older than this"`
	CustomLists  []string `toml:"custom_lists" help:"extra IP lists, each NAME|KIND|URL (kind: ipset, proxylist, torexit, datacenter)"`
	Geo          string   `toml:"geo" help:"geolocation / ASN provider queried directly for exit IPs" choices:"ip-api|none"`
	GeoURL       string   `toml:"geo_url" help:"ip-api compatible batch endpoint"`
	MXToolbox    bool     `toml:"mxtoolbox" help:"also run the mxtoolbox blacklist lookup in full scans (slow, scraped)"`
	WIMIA        bool     `toml:"wimia" help:"also run the whatismyipaddress proxy check in full scans (often Cloudflare-blocked)"`
	Resume       bool     `toml:"resume" help:"resume an interrupted scan of the same mode unless --restart is given"`
	Rank         string   `toml:"rank" help:"order the rank column follows after a scan" choices:"balanced|speed|clean|stable"`
	WeightSpeed  float64  `toml:"weight_speed" help:"weight of the speed score in the balanced rank" min:"0"`
	WeightClean  float64  `toml:"weight_clean" help:"weight of the clean (reputation / anonymity) score in the balanced rank" min:"0"`
	WeightStable float64  `toml:"weight_stable" help:"weight of the stability score in the balanced rank" min:"0"`
	SpeedRefMS   int      `toml:"speed_ref_ms" help:"latency that scores 50/100 on speed (lower = stricter)" min:"1"`
}

// TagsConfig configures the heuristic tags (stable, fast, fav:<cmd>, ...).
type TagsConfig struct {
	Auto          bool     `toml:"auto" help:"refresh the tags of a proxy after every use and of everything after a scan"`
	StableN       int      `toml:"stable_n" help:"stable: the last N checks / uses all succeeded" min:"1"`
	FastN         int      `toml:"fast_n" help:"fast: the last N timed successes ..." min:"1"`
	FastMS        int      `toml:"fast_ms" help:"... were all under this many ms" min:"1"`
	FavMinUses    int      `toml:"fav_min_uses" help:"fav:<cmd>: used at least N times for <cmd> ..." min:"1"`
	FavMinRate    float64  `toml:"fav_min_rate" help:"... with at least this success rate (0-1)" min:"0"`
	RecommendTop  int      `toml:"recommend_top" help:"recommended / recommended:<cmd>: how many proxies get the tag" min:"0"`
	FlakyMinFacts int      `toml:"flaky_min_facts" help:"flaky: at least N facts ..." min:"1"`
	FlakyMaxRate  float64  `toml:"flaky_max_rate" help:"... with a success rate below this (0-1)" min:"0"`
	Window        Duration `toml:"window" help:"usage window for fav / recommended tags (0 = all history)"`
}

// HistoryConfig configures the usage log of proxied commands.
type HistoryConfig struct {
	Record       bool     `toml:"record" help:"record every routed command in the usage log"`
	MeasureOnUse string   `toml:"measure_on_use" help:"measure the proxy on every use: parallel = while the command runs, after = once it exits" choices:"parallel|after|off"`
	TextLog      bool     `toml:"text_log" help:"also append one line per command to <home>/logs/usage.log"`
	Retention    Duration `toml:"retention" help:"delete usage rows older than this (0 = keep forever)"`
	ListLimit    int      `toml:"list_limit" help:"default row limit of 'px history' (0 = all)" min:"0"`
	SkipCommands []string `toml:"skip_commands" help:"commands never logged by the shell hooks"`
}

// ShellConfig configures shell integration (px on / px off, prompt segment).
type ShellConfig struct {
	Verify       bool     `toml:"verify" help:"verify the proxy before 'px on' exports it"`
	NoProxy      string   `toml:"no_proxy" help:"NO_PROXY exported by 'px on'"`
	LogCommands  bool     `toml:"log_commands" help:"log every command typed while the proxy is on (preexec / precmd hooks)"`
	Icon         string   `toml:"icon" help:"prompt icon shown while the proxy is on (Nerd Font glyph or emoji)"`
	Color        string   `toml:"color" help:"prompt color (zsh / p10k color number or name)"`
	PromptFormat string   `toml:"prompt_format" help:"segment text: {icon} {scheme} {host} {port} {ms} {cc} {id} {tags}"`
	PromptAuto   bool     `toml:"prompt_auto" help:"plain zsh / bash / fish: prefix the prompt while the proxy is on"`
	P10K         string   `toml:"p10k" help:"add the 'silkrouter' segment to powerlevel10k automatically" choices:"left|right|none"`
	Method       string   `toml:"method" help:"how 'px shell' proxies its process tree" choices:"env|proxychains"`
	Shells       []string `toml:"shells" help:"shells 'px shell install' writes to when none is given"`
}

// TUIConfig configures the interactive terminal UI.
type TUIConfig struct {
	Theme        string   `toml:"theme" help:"color theme" choices:"silk|mono|neon"`
	Refresh      Duration `toml:"refresh" help:"auto-refresh interval of live views (0 = manual)"`
	PageSize     int      `toml:"page_size" help:"rows loaded per page in tables" min:"10"`
	StartTab     string   `toml:"start_tab" help:"tab shown on start" choices:"dashboard|proxies|history|tags|scans|config"`
	Mouse        bool     `toml:"mouse" help:"enable mouse support"`
	AltScreen    bool     `toml:"alt_screen" help:"use the alternate screen"`
	Columns      []string `toml:"columns" help:"proxy table columns, in order"`
	ConfirmQuit  bool     `toml:"confirm_quit" help:"ask before quitting while a check is running"`
	Markdown     bool     `toml:"markdown" help:"render help and details as markdown (glamour)"`
	DefaultSort  string   `toml:"default_sort" help:"initial sort of the proxy table (px proxies --sort keys)"`
	ShowDeadRows bool     `toml:"show_dead" help:"show dead proxies in the proxy table by default"`
}

// Default returns the built-in defaults.
func Default() *Config {
	c := &Config{
		Source: SourceConfig{Timeout: Seconds(30)},
		Probe:  ProbeConfig{Name: "cloudflare"},
		Route: RouteConfig{
			MaxMS:         1000,
			Scheme:        "any",
			Verify:        "auto",
			VerifyTimeout: Seconds(8),
			Pool:          "db",
			Prefer:        "smart",
			ExcludeTags:   []string{"blocked"},
			Method:        "env",
		},
		Build:      BuildConfig{Workers: 200, Tries: 2, Timeout: Seconds(15)},
		Check:      CheckConfig{Workers: 100, Tries: 1, Timeout: Seconds(8)},
		BLCheck:    BLCheckConfig{Delay: Seconds(2), Timeout: Seconds(20)},
		ProxyCheck: ProxyCheckConfig{Timeout: Seconds(8)},
		UI:         UIConfig{Color: "auto"},
		DB: DBConfig{
			AutoImport:    true,
			BatchSize:     500,
			FlushInterval: Seconds(1),
			BusyTimeout:   Seconds(5),
			Record:        true,
			Backup:        true,
			ListLimit:     50,
			ShowRecent:    15,
		},
		Scan: ScanConfig{
			Workers:      32,
			QuickWorkers: 200,
			Tries:        3,
			Timeout:      Seconds(10),
			Delay:        Duration(150 * time.Millisecond),
			Probes:       []string{"cloudflare", "google", "gstatic", "msft", "apple", "example"},
			FailFast:     2,
			ExitProbe:    "ipify",
			HTTPSURL:     "https://www.cloudflare.com/cdn-cgi/trace",
			JudgeURL:     "http://httpbin.org/get?show_env=1",
			RealIPURL:    "https://api.ipify.org/",
			Protocols:    true,
			DNSBL:        true,
			DNSBLZones: []string{
				"zen.spamhaus.org", "bl.spamcop.net", "b.barracudacentral.org", "dnsbl.dronebl.org",
				"rbl.efnetrbl.org", "psbl.surriel.com", "all.s5h.net", "dnsbl-1.uceprotect.net",
				"ix.dnsbl.manitu.net", "bl.blocklist.de", "torexit.dan.me.uk", "dnsbl.spfbl.net",
				"bl.mailspike.net", "spam.dnsbl.anonmails.de", "rbl.interserver.net",
			},
			DNSTimeout:   Seconds(4),
			Lists:        true,
			ListsTTL:     Duration(12 * time.Hour),
			Geo:          "ip-api",
			GeoURL:       "http://ip-api.com/batch",
			Resume:       true,
			Rank:         "balanced",
			WeightSpeed:  0.4,
			WeightClean:  0.35,
			WeightStable: 0.25,
			SpeedRefMS:   250,
		},
		Tags: TagsConfig{
			Auto:          true,
			StableN:       10,
			FastN:         10,
			FastMS:        100,
			FavMinUses:    10,
			FavMinRate:    0.8,
			RecommendTop:  5,
			FlakyMinFacts: 10,
			FlakyMaxRate:  0.5,
		},
		History: HistoryConfig{
			Record:       true,
			MeasureOnUse: "parallel",
			TextLog:      true,
			ListLimit:    50,
			SkipCommands: []string{"px", "s-proxy", "silkrouter", "cd", "ls", "clear", "exit"},
		},
		Shell: ShellConfig{
			Verify:       true,
			NoProxy:      "localhost,127.0.0.1,::1",
			LogCommands:  true,
			Icon:         "\U000f048d", // nf-md-web (󰒍)
			Color:        "208",
			PromptFormat: "{icon} {scheme} {host}:{port} {ms} {cc}",
			PromptAuto:   true,
			P10K:         "left",
			Method:       "env",
			Shells:       []string{"zsh"},
		},
		TUI: TUIConfig{
			Theme:       "silk",
			Refresh:     Seconds(5),
			PageSize:    500,
			StartTab:    "dashboard",
			Mouse:       true,
			AltScreen:   true,
			Columns:     []string{"id", "proxy", "status", "last", "avg", "ok", "fail", "streak", "cc", "anon", "bl", "rank", "tags"},
			ConfirmQuit: true,
			Markdown:    true,
			DefaultSort: "rank",
		},
	}
	c.meta.sources = map[string]string{}
	return c
}

// Source of a value, as reported by Config.Origin.
const (
	FromDefault = "default"
	FromFile    = "file"
)

type meta struct {
	path     string
	exists   bool
	sources  map[string]string // key -> FromDefault | FromFile | "env NAME"
	warnings []string
}

// Path is the file this config was loaded from ("" for Default()).
func (c *Config) Path() string { return c.meta.path }

// FileExists reports whether the config file existed when loaded.
func (c *Config) FileExists() bool { return c.meta.exists }

// Warnings are non-fatal problems found while loading (unknown keys,
// invalid values replaced by defaults, ...).
func (c *Config) Warnings() []string { return c.meta.warnings }

// Origin reports where key's effective value came from: "default", "file"
// or "env NAME".
func (c *Config) Origin(key string) string {
	if s, ok := c.meta.sources[key]; ok {
		return s
	}
	return FromDefault
}

func (c *Config) warnf(format string, a ...any) {
	c.meta.warnings = append(c.meta.warnings, fmt.Sprintf(format, a...))
}

// CacheFile resolves the active text-cache path: cache.file (or $PX_LIST,
// via the env layer) when set, else <home>/cache.list.
func (c *Config) CacheFile() string {
	if c.Cache.File != "" {
		return paths.ExpandUser(c.Cache.File)
	}
	return paths.LegacyCache()
}

// DBFile resolves the database path: db.path when set, else
// <home>/silkrouter.db.
func (c *Config) DBFile() string {
	if c.DB.Path != "" {
		return paths.ExpandUser(c.DB.Path)
	}
	return paths.DB()
}

// Load resolves defaults < file < environment. A missing file is not an
// error. On a parse error the returned config holds defaults + environment
// (still usable) and err describes the problem. lookupEnv is usually
// os.LookupEnv; nil skips the environment layer.
func Load(path string, lookupEnv func(string) (string, bool)) (*Config, error) {
	c, err := LoadFile(path)
	if lookupEnv != nil {
		c.ApplyEnv(lookupEnv)
	}
	return c, err
}

// LoadFile resolves defaults < file only. Use it for anything that writes the
// file back (config set/reset), so environment overrides never get baked in.
func LoadFile(path string) (*Config, error) {
	c := Default()
	c.meta.path = path
	if path == "" {
		return c, nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	c.meta.exists = true

	fileCfg := Default()
	md, err := toml.DecodeFile(path, fileCfg)
	if err != nil {
		var perr toml.ParseError
		if errors.As(err, &perr) {
			return c, fmt.Errorf("config %s: %s", path, strings.TrimSpace(perr.ErrorWithPosition()))
		}
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	fileCfg.meta = c.meta
	c = fileCfg

	for _, u := range md.Undecoded() {
		c.warnf("%s: unknown key %q (ignored)", path, u.String())
	}
	for _, k := range md.Keys() {
		name := k.String()
		if _, ok := Lookup(name); ok {
			c.meta.sources[name] = FromFile
		}
	}
	// Invalid values fall back to the default instead of failing the run.
	def := Default()
	for _, k := range Keys() {
		if err := c.validate(k); err != nil {
			c.warnf("%s: %v (using default %s)", path, err, k.Default)
			k.copy(c, def)
			delete(c.meta.sources, k.Name)
		}
	}
	return c, nil
}

// ApplyEnv overlays environment variables: SILKROUTER_<KEY> first, then
// the key's legacy names. Empty variables count as unset; invalid values are
// ignored with a warning.
func (c *Config) ApplyEnv(lookupEnv func(string) (string, bool)) {
	for _, k := range Keys() {
		for _, name := range k.Env {
			v, ok := lookupEnv(name)
			if !ok || v == "" {
				continue
			}
			if err := c.Set(k.Name, v); err != nil {
				c.warnf("$%s: %v (ignored)", name, err)
				continue
			}
			c.meta.sources[k.Name] = "env " + name
			break
		}
	}
}

// Seconds is a convenience constructor for whole-second durations.
func Seconds(n int) Duration { return Duration(time.Duration(n) * time.Second) }
