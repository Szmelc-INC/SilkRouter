// Package paths resolves SilkRouter's config home and every file inside it.
//
// Home() resolves, in order:
//
//  1. $SILKROUTER_HOME (tests always set this; --home sets it too),
//  2. ${XDG_CONFIG_HOME:-$HOME/.config}/szmelc/proxy.
//
// Every other helper is a fixed location inside Home (see the layout table in
// docs/ARCHITECTURE-v4.md). Nothing here touches the filesystem except
// Ensure.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// EnvHome overrides the config home. Tests must always set it.
const EnvHome = "SILKROUTER_HOME"

// EnvConfig overrides the config file location (the --config flag wins).
const EnvConfig = "SILKROUTER_CONFIG"

// EnvLegacyList is the px.sh-era text-cache override, still honoured.
const EnvLegacyList = "PX_LIST"

// Names of the entries inside Home.
const (
	ConfigFile    = "config.toml"
	DBFile        = "silkrouter.db"
	ListsDirName  = "lists"
	LogsDirName   = "logs"
	ShellDirName  = "shell"
	RunDirName    = "run"
	CacheListFile = "cache.list"
)

// DirPerm is the mode for every directory Ensure creates.
const DirPerm os.FileMode = 0o700

// Home returns the resolved config home (not necessarily existing).
func Home() string {
	if v := os.Getenv(EnvHome); v != "" {
		return ExpandUser(v)
	}
	return filepath.Join(configBase(), "szmelc", "proxy")
}

// HomeSource describes where Home() came from: "$SILKROUTER_HOME",
// "$XDG_CONFIG_HOME" or "default".
func HomeSource() string {
	switch {
	case os.Getenv(EnvHome) != "":
		return "$" + EnvHome
	case os.Getenv("XDG_CONFIG_HOME") != "":
		return "$XDG_CONFIG_HOME"
	default:
		return "default"
	}
}

// configBase is ${XDG_CONFIG_HOME:-$HOME/.config}.
func configBase() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" && filepath.IsAbs(v) {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Last resort so we never write into the working directory.
		return filepath.Join(os.TempDir(), "silkrouter-"+userName())
	}
	return filepath.Join(home, ".config")
}

func userName() string {
	for _, k := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "user"
}

// Config is the default config file path. The --config flag and
// $SILKROUTER_CONFIG override it (see ConfigPath).
func Config() string { return filepath.Join(Home(), ConfigFile) }

// ConfigPath returns the config file to use: flagValue if set, else
// $SILKROUTER_CONFIG, else Config().
func ConfigPath(flagValue string) string {
	if flagValue != "" {
		return ExpandUser(flagValue)
	}
	if v := os.Getenv(EnvConfig); v != "" {
		return ExpandUser(v)
	}
	return Config()
}

// DB is the SQLite database path.
func DB() string { return filepath.Join(Home(), DBFile) }

// ListsDir holds downloaded reputation / known-proxy lists.
func ListsDir() string { return filepath.Join(Home(), ListsDirName) }

// LogsDir holds plain-text logs.
func LogsDir() string { return filepath.Join(Home(), LogsDirName) }

// ShellDir holds generated shell init snippets.
func ShellDir() string { return filepath.Join(Home(), ShellDirName) }

// RunDir holds runtime state (session files, sockets, pids).
func RunDir() string { return filepath.Join(Home(), RunDirName) }

// LegacyCache is the text cache inside Home (`scheme://host:port [Nms]`).
// It is the default for route/build/list unless $PX_LIST or -f is given.
func LegacyCache() string { return filepath.Join(Home(), CacheListFile) }

// TempCache is where SilkRouter v3 kept its cache by default.
func TempCache() string { return filepath.Join(os.TempDir(), "silkrouter-proxy.list") }

// LegacyCandidates lists pre-v4 cache locations worth importing on first
// run, most specific first: $PX_LIST, then the v3 temp cache. The px.sh
// default (<tmp>/proxy.list) is deliberately not included: the name is too
// generic to trust; px.sh users can point $PX_LIST at it.
func LegacyCandidates() []string {
	var out []string
	if v := os.Getenv(EnvLegacyList); v != "" {
		out = append(out, ExpandUser(v))
	}
	return append(out, TempCache())
}

// Dirs returns every directory Ensure creates, Home first.
func Dirs() []string {
	return []string{Home(), ListsDir(), LogsDir(), ShellDir(), RunDir()}
}

// Ensure creates Home and its sub-directories with mode 0700. Existing
// directories are left alone (their mode is not changed).
func Ensure() error {
	var errs []error
	for _, d := range Dirs() {
		if err := os.MkdirAll(d, DirPerm); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Entry is one row of the resolved layout.
type Entry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Kind   string `json:"kind"` // "dir" or "file"
	Desc   string `json:"desc"`
	Exists bool   `json:"exists"`
}

// Layout returns the resolved layout in contract order. configPath is the
// config file actually in use (it may live outside Home).
func Layout(configPath string) []Entry {
	if configPath == "" {
		configPath = Config()
	}
	rows := []Entry{
		{Name: "home", Path: Home(), Kind: "dir", Desc: "config home (" + HomeSource() + ")"},
		{Name: "config", Path: configPath, Kind: "file", Desc: "user configuration (TOML)"},
		{Name: "db", Path: DB(), Kind: "file", Desc: "SQLite database"},
		{Name: "lists", Path: ListsDir(), Kind: "dir", Desc: "downloaded reputation / proxy lists"},
		{Name: "logs", Path: LogsDir(), Kind: "dir", Desc: "plain-text logs"},
		{Name: "shell", Path: ShellDir(), Kind: "dir", Desc: "generated shell init snippets"},
		{Name: "run", Path: RunDir(), Kind: "dir", Desc: "runtime state (sessions, sockets, pids)"},
		{Name: "cache", Path: LegacyCache(), Kind: "file", Desc: "legacy text cache"},
	}
	for i := range rows {
		_, err := os.Stat(rows[i].Path)
		rows[i].Exists = err == nil
	}
	return rows
}

// ExpandUser expands a leading "~" or "~/" to the user's home directory.
func ExpandUser(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// Pretty abbreviates the user's home directory to "~" for display.
func Pretty(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || p == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}
