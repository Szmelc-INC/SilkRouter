package shell

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Markers delimit the block 'px shell install' manages in rc files.
const (
	BlockStart = "# >>> silkrouter shell integration >>>"
	BlockEnd   = "# <<< silkrouter shell integration <<<"
)

// RCFile is the default rc file of sh under home.
func RCFile(sh, home string) string {
	switch sh {
	case "bash":
		return filepath.Join(home, ".bashrc")
	case "fish":
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
			return filepath.Join(x, "fish", "config.fish")
		}
		return filepath.Join(home, ".config", "fish", "config.fish")
	default:
		if z := os.Getenv("ZDOTDIR"); z != "" && filepath.IsAbs(z) {
			return filepath.Join(z, ".zshrc")
		}
		return filepath.Join(home, ".zshrc")
	}
}

// Block renders the rc block for sh: source the generated snippet when it
// exists (fast), else eval the binary's output.
func Block(sh, snippet, bin string) string {
	var body string
	switch sh {
	case "fish":
		body = fmt.Sprintf("if test -r %s\n    source %s\nelse if command -q %s\n    %s shell-init fish | source\nend",
			QuoteFish(snippet), QuoteFish(snippet), QuoteFish(bin), QuoteFish(bin))
	default:
		body = fmt.Sprintf("if [ -r %s ]; then\n  . %s\nelif command -v %s >/dev/null 2>&1; then\n  eval \"$(%s shell-init %s)\"\nfi",
			QuoteSh(snippet), QuoteSh(snippet), QuoteSh(bin), QuoteSh(bin), sh)
	}
	return BlockStart + "\n# Managed by 'silkrouter shell install'; remove with 'silkrouter shell uninstall'.\n" +
		body + "\n" + BlockEnd + "\n"
}

// Splice returns content with the managed block replaced by block (or
// removed when block is ""), and whether a block was there before.
func Splice(content, block string) (string, bool) {
	start := strings.Index(content, BlockStart)
	if start < 0 {
		if block == "" {
			return content, false
		}
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if content != "" {
			content += "\n"
		}
		return content + block, false
	}
	end := strings.Index(content[start:], BlockEnd)
	if end < 0 {
		end = len(content) - start
	} else {
		end += len(BlockEnd)
		if start+end < len(content) && content[start+end] == '\n' {
			end++
		}
	}
	out := content[:start] + block + content[start+end:]
	if block == "" {
		out = strings.TrimRight(out, "\n") + "\n"
		if strings.TrimSpace(out) == "" {
			out = ""
		}
	}
	return out, true
}

// InstallResult reports Install / Uninstall.
type InstallResult struct {
	File    string `json:"file"`
	Backup  string `json:"backup,omitempty"`
	Changed bool   `json:"changed"`
	Existed bool   `json:"existed"` // a block was there already
}

// Install writes (or refreshes) the block in file, backing the old file up
// as file.silkrouter-bak-<time> unless noBackup.
func Install(file, block string, noBackup, dryRun bool) (InstallResult, error) {
	return rewrite(file, block, noBackup, dryRun)
}

// Uninstall removes the block from file.
func Uninstall(file string, noBackup, dryRun bool) (InstallResult, error) {
	return rewrite(file, "", noBackup, dryRun)
}

func rewrite(file, block string, noBackup, dryRun bool) (InstallResult, error) {
	res := InstallResult{File: file}
	old, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	out, existed := Splice(string(old), block)
	res.Existed = existed
	res.Changed = out != string(old)
	if !res.Changed || dryRun {
		return res, nil
	}
	if len(old) > 0 && !noBackup {
		res.Backup = file + ".silkrouter-bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(res.Backup, old, 0o600); err != nil {
			return res, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return res, err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(file); err == nil {
		mode = fi.Mode().Perm()
	}
	return res, os.WriteFile(file, []byte(out), mode)
}

// ---- px shell bootstrap --------------------------------------------------------

// Bootstrap prepares how `px shell` starts sh with the integration loaded
// on top of the user's own rc: extra arguments and environment. dir is a
// private directory (removed by the caller afterwards); snippet the init
// file.
func Bootstrap(sh, dir, snippet string) (args []string, env []string, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	switch sh {
	case "zsh":
		orig := os.Getenv("ZDOTDIR")
		if orig == "" {
			orig, _ = os.UserHomeDir()
		}
		zshenv := "ZDOTDIR=" + QuoteSh(orig) + "\n[ -r \"$ZDOTDIR/.zshenv\" ] && . \"$ZDOTDIR/.zshenv\"\nZDOTDIR=" + QuoteSh(dir) + "\n"
		zshrc := "ZDOTDIR=" + QuoteSh(orig) + "\n" +
			"[ -r \"$ZDOTDIR/.zshrc\" ] && . \"$ZDOTDIR/.zshrc\"\n" +
			"(( ${+functions[__silkrouter_wrap]} )) || . " + QuoteSh(snippet) + "\n"
		if err := os.WriteFile(filepath.Join(dir, ".zshenv"), []byte(zshenv), 0o600); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, ".zshrc"), []byte(zshrc), 0o600); err != nil {
			return nil, nil, err
		}
		return []string{"-i"}, []string{"ZDOTDIR=" + dir}, nil
	case "bash":
		rc := filepath.Join(dir, "bashrc")
		body := "[ -r \"$HOME/.bashrc\" ] && . \"$HOME/.bashrc\"\n" +
			"declare -F __silkrouter_wrap >/dev/null 2>&1 || . " + QuoteSh(snippet) + "\n"
		if err := os.WriteFile(rc, []byte(body), 0o600); err != nil {
			return nil, nil, err
		}
		return []string{"--rcfile", rc, "-i"}, nil, nil
	case "fish":
		return []string{"-i", "-C", "functions -q __silkrouter_wrap; or source " + QuoteFish(snippet)}, nil, nil
	}
	return nil, nil, nil
}
