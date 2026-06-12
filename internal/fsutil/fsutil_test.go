package fsutil

import (
	"path/filepath"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	root := filepath.FromSlash("/srv/ws")

	good := map[string]string{
		"a.txt":       "/srv/ws/a.txt",
		"sub/b.txt":   "/srv/ws/sub/b.txt",
		"sub/../c":    "/srv/ws/c",
		"./d":         "/srv/ws/d",
		"sub//e.bin":  "/srv/ws/sub/e.bin",
		"sub/./f.txt": "/srv/ws/sub/f.txt",
	}
	for rel, want := range good {
		got, err := SafeJoin(root, rel)
		if err != nil {
			t.Errorf("SafeJoin(%q) failed: %v", rel, err)
			continue
		}
		if got != filepath.FromSlash(want) {
			t.Errorf("SafeJoin(%q) = %q, want %q", rel, got, want)
		}
	}

	bad := []string{"../evil", "a/../../evil", "..", "/abs.txt", "sub/../../../etc/passwd"}
	for _, rel := range bad {
		if got, err := SafeJoin(root, rel); err == nil {
			t.Errorf("SafeJoin(%q) = %q, want rejection", rel, got)
		}
	}
}
