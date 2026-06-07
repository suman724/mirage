package index

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/suman724/mirage/internal/chunk"
)

func TestIsSecret(t *testing.T) {
	secrets := []string{".env", ".env.local", "server.pem", "tls.key", "id_rsa", "id_ed25519", ".netrc", "credentials"}
	for _, n := range secrets {
		if !IsSecret(n) {
			t.Errorf("%q should be a secret", n)
		}
	}
	for _, n := range []string{"main.go", "README.md", "config.yaml", "envoy.yaml"} {
		if IsSecret(n) {
			t.Errorf("%q should NOT be a secret", n)
		}
	}
}

func TestBuildExcludesSecretsAndGit(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "src/main.go", "package main\n")
	mustWrite(t, root, "src/dup.go", "package main\n") // duplicate content
	mustWrite(t, root, ".env", "SECRET=1\n")
	mustWrite(t, root, "id_rsa", "PRIVATE\n")
	mustWrite(t, root, ".git/config", "[core]\n")
	mustWrite(t, root, "node_modules/dep/index.js", "module.exports={}\n")

	m, store, err := Build(root)
	if err != nil {
		t.Fatal(err)
	}

	paths := map[string]bool{}
	for _, f := range m.Files {
		paths[f.Path] = true
	}
	if !paths["src/main.go"] || !paths["src/dup.go"] {
		t.Fatalf("expected source files in manifest, got %v", paths)
	}
	for _, banned := range []string{".env", "id_rsa", ".git/config", "node_modules/dep/index.js"} {
		if paths[banned] {
			t.Errorf("excluded path %q leaked into manifest", banned)
		}
	}

	// The store must hold exactly the manifest's hashes, and the duplicate
	// file must dedup to a single chunk shared by both entries.
	for h := range m.UniqueHashes() {
		if !store.Has(h) {
			t.Errorf("manifest hash %s missing from store", h)
		}
	}
	if store.Len() != len(m.UniqueHashes()) {
		t.Errorf("store has %d chunks, manifest has %d unique", store.Len(), len(m.UniqueHashes()))
	}

	// Confirm the secret's chunk is genuinely absent from the store.
	secretHash := chunk.HashOf([]byte("SECRET=1\n"))
	if store.Has(secretHash) {
		t.Errorf("secret chunk must never enter the store")
	}
}

func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
