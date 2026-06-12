// Package fsutil holds small filesystem helpers shared by server components
// that materialize manifest paths onto a real filesystem (reconstruct mode,
// shim mode). Manifest paths are untrusted input: they arrive from the client
// over the wire, so every join onto a server-side root must reject traversal.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SafeJoin joins a slash-separated relative manifest path onto root, rejecting
// any path that escapes root (absolute paths, ".." traversal). The returned
// path is cleaned and uses the platform separator.
func SafeJoin(root, rel string) (string, error) {
	if filepath.IsAbs(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("fsutil: path %q must be relative to root", rel)
	}
	clean := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	rootAbs := filepath.Clean(root)
	if clean != rootAbs && !strings.HasPrefix(clean, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("fsutil: path %q escapes root %q", rel, root)
	}
	return clean, nil
}
