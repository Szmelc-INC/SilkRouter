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
	File string `toml:"file" help:"text cache file; empty means <home>/cache.list" env:"PX_LIST"`
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

// DBConfig is reserved for the database settings.
type DBConfig struct{}

// ScanConfig is reserved for full-scan settings.
type ScanConfig struct{}

// TagsConfig is reserved for heuristic tag thresholds.
type TagsConfig struct{}

// HistoryConfig is reserved for usage-log settings.
type HistoryConfig struct{}

// ShellConfig is reserved for shell-integration settings.
type ShellConfig struct{}

// TUIConfig is reserved for TUI settings.
type TUIConfig struct{}

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
		},
		Build:      BuildConfig{Workers: 200, Tries: 2, Timeout: Seconds(15)},
		Check:      CheckConfig{Workers: 100, Tries: 1, Timeout: Seconds(8)},
		BLCheck:    BLCheckConfig{Delay: Seconds(2), Timeout: Seconds(20)},
		ProxyCheck: ProxyCheckConfig{Timeout: Seconds(8)},
		UI:         UIConfig{Color: "auto"},
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
