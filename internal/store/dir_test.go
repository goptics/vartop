package store

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPathForSafeAndUnique(t *testing.T) {
	dir := t.TempDir()
	keys := []string{"prod", "Prod", "a/b", "x y", "ünï", strings.Repeat("k", 100), "", "..", `C:\x`}
	safe := regexp.MustCompile(`^[a-z0-9._-]+$`)
	seen := make(map[string]string)
	for _, key := range keys {
		p := PathFor(dir, key)
		if again := PathFor(dir, key); again != p {
			t.Errorf("PathFor(%q) not deterministic: %q then %q", key, p, again)
		}
		if got := filepath.Dir(p); got != filepath.Join(dir, "servers") {
			t.Errorf("PathFor(%q) = %q, want a file in %q", key, p, filepath.Join(dir, "servers"))
		}
		name := filepath.Base(p)
		if !safe.MatchString(name) {
			t.Errorf("PathFor(%q) file name %q has characters outside [a-z0-9._-]", key, name)
		}
		if len(name) > 40+1+6+3 {
			t.Errorf("PathFor(%q) file name %q is %d chars, want ≤ 50", key, name, len(name))
		}
		if !strings.HasSuffix(name, ".db") {
			t.Errorf("PathFor(%q) file name %q does not end in .db", key, name)
		}
		// Case-insensitive filesystems: names must differ beyond case.
		folded := strings.ToLower(name)
		if other, ok := seen[folded]; ok {
			t.Errorf("PathFor(%q) and PathFor(%q) share the file name %q", key, other, name)
		}
		seen[folded] = key
	}
	if got := filepath.Base(PathFor(dir, "Prod")); !strings.HasPrefix(got, "prod-") {
		t.Errorf("PathFor(Prod) = %q, want the lowercased key first", got)
	}
}

func TestDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARTOP_DATA_DIR", dir)
	got, err := DataDir()
	if err != nil || got != dir {
		t.Errorf("DataDir() = %q, %v; want %q", got, err, dir)
	}
}
