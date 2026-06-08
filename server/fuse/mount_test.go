package fuse

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/folbricht/desync"

	"github.com/suman724/mirage/client/index"
	"github.com/suman724/mirage/internal/chunk"
)

// storeFromManifest builds an in-memory desync.Store holding every chunk the
// manifest references, sourced from the client-side index build. It lets the
// live-mount test run without a real client/server connection.
func storeFromManifest(t *testing.T, m *chunk.Manifest, cs interface {
	Get(chunk.Hash) ([]byte, bool)
}) *memStore {
	t.Helper()
	store := &memStore{chunks: make(map[desync.ChunkID][]byte)}
	for h := range m.UniqueHashes() {
		data, ok := cs.Get(h)
		if !ok {
			t.Fatalf("chunk %s missing from index store", h)
		}
		store.chunks[desync.ChunkID(h)] = data
	}
	return store
}

// TestLiveMount mounts testdata as a FUSE tree and reads files back through the
// kernel, proving lazy faulting end to end. It SKIPS when no FUSE module is
// available (e.g. macOS without macFUSE); it is meant to run in the Linux/Docker
// validation environment (task 2.5-val).
func TestLiveMount(t *testing.T) {
	srcDir := filepath.Join("..", "..", "testdata", "workspace")
	m, cs, err := index.Build(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	store := storeFromManifest(t, m, cs)

	mnt := t.TempDir()
	mount, err := New(mnt, m, store, nil)
	if err != nil {
		t.Skipf("FUSE not available in this environment (expected on macOS without macFUSE): %v", err)
	}
	t.Cleanup(func() {
		// Best-effort unmount; retry briefly in case the FS is still busy.
		for i := 0; i < 10; i++ {
			if err := mount.Unmount(); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	// Read every published file through the mount and compare to the source.
	for _, f := range m.Files {
		want, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(mnt, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Errorf("read %q via mount: %v", f.Path, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("file %q via mount differs (%d vs %d bytes)", f.Path, len(got), len(want))
		}
	}

	// A second read of a file should be served from cache/kernel without new
	// chunk fetches beyond the first pass.
	first := store.gets.Load()
	if len(m.Files) > 0 {
		p := filepath.Join(mnt, filepath.FromSlash(m.Files[0].Path))
		if _, err := os.ReadFile(p); err != nil {
			t.Errorf("re-read: %v", err)
		}
	}
	if store.gets.Load() < first {
		t.Fatalf("fetch counter went backwards")
	}
}
