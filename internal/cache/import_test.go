package cache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/szmelc-inc/silkrouter/internal/paths"
)

func TestImportLegacy(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.list")
	os.WriteFile(empty, []byte("# nothing\njunk\n"), 0o600)
	good := filepath.Join(dir, "good.list")
	os.WriteFile(good, []byte("socks5://1.2.3.4:1080 [120ms]\nhttp://5.6.7.8:3128 [80ms]\n"), 0o600)
	dst := filepath.Join(dir, "home", "cache.list")

	src, n, err := ImportLegacy(dst, []string{filepath.Join(dir, "missing"), empty, good})
	if err != nil || src != good || n != 2 {
		t.Fatalf("ImportLegacy = %q, %d, %v", src, n, err)
	}
	ps, err := Load(dst)
	if err != nil || len(ps) != 2 || ps[0].LatencyMS != 80 {
		t.Fatalf("imported cache: %v %v", ps, err)
	}

	// An existing destination is never replaced.
	os.WriteFile(good, []byte("http://9.9.9.9:80 [1ms]\n"), 0o600)
	if src, _, _ := ImportLegacy(dst, []string{good}); src != "" {
		t.Fatalf("re-imported over an existing cache from %s", src)
	}
}

func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv("PX_LIST", "")
	if got := DefaultPath(); got != filepath.Join(home, "cache.list") {
		t.Fatalf("DefaultPath = %q", got)
	}
	t.Setenv("PX_LIST", "/x/y.list")
	if got := DefaultPath(); got != "/x/y.list" {
		t.Fatalf("DefaultPath with PX_LIST = %q", got)
	}
}
