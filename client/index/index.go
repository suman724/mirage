// Package index builds a Mirage workspace index: it walks a directory, applies
// the ignore + secret-exclusion policy (design §6), chunks each eligible file,
// and produces both the publishable manifest and a populated chunkstore.
//
// The exclusion policy is the security boundary: secret files are never
// chunked, so their chunks never enter the manifest or the store and can never
// be requested by the server.
package index

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/suman724/mirage/client/chunkstore"
	"github.com/suman724/mirage/internal/chunk"
)

// excludedDirs are directory names skipped entirely during indexing.
var excludedDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
}

// IsSecret reports whether a base filename matches the secret denylist and must
// never be indexed (design §6: .env*, *.pem, id_*, credential-like files).
func IsSecret(name string) bool {
	switch {
	case strings.HasPrefix(name, ".env"):
		return true
	case strings.HasSuffix(name, ".pem"):
		return true
	case strings.HasSuffix(name, ".key"):
		return true
	case strings.HasPrefix(name, "id_"): // id_rsa, id_ed25519, ...
		return true
	case name == ".netrc" || name == "credentials":
		return true
	}
	return false
}

// Build walks root, chunks eligible files, and returns the manifest plus a
// chunkstore populated with every chunk in the manifest. The two are
// consistent by construction: the store serves exactly the hashes the manifest
// publishes.
func Build(root string) (*chunk.Manifest, *chunkstore.Store, error) {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("index: root %q is not a directory", root)
	}

	manifest := &chunk.Manifest{}
	store := chunkstore.New()

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := excludedDirs[d.Name()]; skip && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		// Only regular files are chunked (skip symlinks, sockets, devices).
		if !d.Type().IsRegular() {
			return nil
		}
		if IsSecret(d.Name()) {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}

		refs, chunks, err := chunk.Split(data)
		if err != nil {
			return fmt.Errorf("chunk %s: %w", rel, err)
		}
		for h, b := range chunks {
			store.Put(h, b)
		}
		manifest.Files = append(manifest.Files, chunk.FileEntry{
			Path:   rel,
			Mode:   uint32(fi.Mode().Perm()),
			Chunks: refs,
		})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return manifest, store, nil
}
