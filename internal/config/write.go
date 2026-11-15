package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// WriteOptions controls Write.
type WriteOptions struct {
	// Docs adds the file header and a help comment above every key.
	Docs bool
	// CommentDefaults writes keys that equal their default commented out, so
	// the file only pins what the user changed (config-file mode).
	CommentDefaults bool
	// Sources appends "# <source>" to every key (for `config show --sources`).
	Sources bool
}

// FileHeader opens every generated config file.
const FileHeader = `# SilkRouter configuration (TOML).
#
# Precedence: built-in defaults < this file < environment < command-line flags.
# Every key can also be set from the environment as SILKROUTER_<TABLE>_<KEY>,
# e.g. route.max_ms -> SILKROUTER_ROUTE_MAX_MS. Legacy PX_LIST (cache.file)
# and MXTB_TOKEN (blcheck.token) still work.
#
# Keys that are commented out use the built-in default shown.
# Durations: seconds (8, 0.5) or strings ("8s", "500ms", "1m30s").
# Manage this file with: px config get|set|edit|reset|show
`

// Write renders c as TOML.
func Write(w io.Writer, c *Config, opt WriteOptions) error {
	var b bytes.Buffer
	if opt.Docs {
		b.WriteString(FileHeader)
	}
	for ti, t := range Tables() {
		if ti > 0 || opt.Docs {
			b.WriteByte('\n')
		}
		if opt.Docs && t.Help != "" {
			fmt.Fprintf(&b, "# %s\n", t.Help)
		}
		fmt.Fprintf(&b, "[%s]\n", t.Name)
		if len(t.Keys) == 0 && opt.Docs {
			b.WriteString("# (no settings yet)\n")
		}
		for _, k := range t.Keys {
			line, err := keyLine(k, c.field(k).Interface())
			if err != nil {
				return err
			}
			if opt.Docs {
				for _, l := range keyDoc(k) {
					fmt.Fprintf(&b, "# %s\n", l)
				}
			}
			if opt.CommentDefaults && c.IsDefault(k.Name) {
				line = "# " + line
			}
			if opt.Sources {
				line += "  # " + c.Origin(k.Name)
			}
			b.WriteString(line + "\n")
		}
	}
	_, err := w.Write(b.Bytes())
	return err
}

// keyDoc is the comment block above a key in the config file.
func keyDoc(k Key) []string {
	doc := []string{k.Help}
	var extra []string
	if len(k.Choices) > 0 {
		extra = append(extra, "one of: "+strings.Join(k.Choices, ", "))
	}
	extra = append(extra, "env: "+strings.Join(k.Env, ", "))
	return append(doc, strings.Join(extra, "; "))
}

// keyLine renders `field = value` using the TOML encoder for correct quoting.
func keyLine(k Key, v any) (string, error) {
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(map[string]any{k.Field: v}); err != nil {
		return "", fmt.Errorf("encode %s: %w", k.Name, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// Save writes c to path atomically (temp file + rename) in config-file
// mode: documented, defaults commented out. The file is created 0600 (it may
// hold a token) and its directory 0700.
func Save(path string, c *Config) error {
	var b bytes.Buffer
	if err := Write(&b, c, WriteOptions{Docs: true, CommentDefaults: true}); err != nil {
		return err
	}
	return WriteFileAtomic(path, b.Bytes(), 0o600)
}

// WriteDefault writes a fully commented default config to path. It refuses
// to replace an existing file unless force is set.
func WriteDefault(path string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return Save(path, Default())
}

// Backup copies path to path+".bak" if it exists and returns the backup
// path ("" when there was nothing to back up).
func Backup(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	bak := path + ".bak"
	return bak, WriteFileAtomic(bak, data, 0o600)
}

// WriteFileAtomic writes data to a temp file beside path and renames it
// into place, so readers never see a half-written file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
