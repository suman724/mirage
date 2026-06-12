package shim

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/folbricht/desync"

	"github.com/suman724/mirage/internal/chunk"
)

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// memStore is an in-memory desync.Store standing in for the
// cache->dedup->channelstore chain, with a fetch counter for laziness
// assertions (the memStore.gets pattern from server/fuse).
type memStore struct {
	chunks map[desync.ChunkID][]byte
	gets   atomic.Uint64
}

func (m *memStore) GetChunk(id desync.ChunkID) (*desync.Chunk, error) {
	m.gets.Add(1)
	data, ok := m.chunks[id]
	if !ok {
		return nil, desync.ChunkMissing{ID: id}
	}
	return desync.NewChunkWithID(id, data, false)
}
func (m *memStore) HasChunk(id desync.ChunkID) (bool, error) { _, ok := m.chunks[id]; return ok, nil }
func (m *memStore) Close() error                             { return nil }
func (m *memStore) String() string                           { return "memstore" }

// buildManifest chunks the given files exactly like the client indexer and
// returns the manifest (sorted by path) plus a store holding every chunk.
func buildManifest(t *testing.T, files map[string][]byte) (*chunk.Manifest, *memStore) {
	t.Helper()
	store := &memStore{chunks: make(map[desync.ChunkID][]byte)}
	m := &chunk.Manifest{}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		refs, chunks, err := chunk.Split(files[p])
		if err != nil {
			t.Fatalf("split %s: %v", p, err)
		}
		for h, b := range chunks {
			store.chunks[desync.ChunkID(h)] = b
		}
		m.Files = append(m.Files, chunk.FileEntry{Path: p, Mode: 0o644, Chunks: refs})
	}
	return m, store
}

// fixtureContent is a deterministic multi-chunk corpus with sentinel bytes
// (no zeros), so reading placeholder zeros is always detectable.
func fixtureContent() map[string][]byte {
	big := make([]byte, 300*1024)
	for i := range big {
		big[i] = byte(i%251) + 1 // 1..251, never 0
	}
	return map[string][]byte{
		"hello.txt":     []byte("MIRAGE_SENTINEL hello\n"),
		"sub/dir/big":   big,
		"sub/other.txt": []byte("MIRAGE_SENTINEL other\n"),
		"empty":         {},
	}
}

// uniqueChunks counts the distinct chunk hashes of one manifest entry.
func uniqueChunks(m *chunk.Manifest, rel string) int {
	for _, f := range m.Files {
		if f.Path == rel {
			set := map[chunk.Hash]struct{}{}
			for _, r := range f.Chunks {
				set[r.Hash] = struct{}{}
			}
			return len(set)
		}
	}
	return -1
}

// env is one fully assembled shim environment: skeleton on disk, journal,
// supervisor listening on a unix socket.
type env struct {
	root      string // symlink-resolved workspace root
	stateDir  string
	sock      string
	files     map[string][]byte
	manifest  *chunk.Manifest
	store     *memStore
	table     *Table
	sup       *Supervisor
	buildTime time.Time
}

// shortTempDir returns a temp dir with a short absolute path: unix socket
// paths are limited to ~104 bytes on darwin, and t.TempDir() can exceed it.
func shortTempDir(t *testing.T, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// newEnv builds the full S1 stack against an in-memory chunk store.
func newEnv(t *testing.T) *env {
	t.Helper()
	files := fixtureContent()
	manifest, store := buildManifest(t, files)

	root := shortTempDir(t, "mshim-root-")
	stateDir := shortTempDir(t, "mshim-state-")

	table, err := OpenTable(filepath.Join(stateDir, "journal.jsonl"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { table.Close() })

	buildTime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	if _, err := BuildSkeleton(root, manifest, table, buildTime, quietLogger()); err != nil {
		t.Fatal(err)
	}

	sup := startSupervisor(t, root, stateDir, manifest, store, table, buildTime)
	return &env{
		root: sup.Root(), stateDir: stateDir, sock: sup.SocketPath(),
		files: files, manifest: manifest, store: store, table: table,
		sup: sup, buildTime: buildTime,
	}
}

func startSupervisor(t *testing.T, root, stateDir string, m *chunk.Manifest, store desync.Store, table *Table, buildTime time.Time) *Supervisor {
	t.Helper()
	sup, err := NewSupervisor(Config{
		Root:       root,
		SocketPath: filepath.Join(stateDir, "s.sock"),
		Manifest:   m,
		Store:      store,
		Table:      table,
		BuildTime:  buildTime,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- sup.Serve() }()
	t.Cleanup(func() {
		if err := sup.Close(); err != nil {
			t.Errorf("close supervisor: %v", err)
		}
		if err := <-serveErr; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return sup
}

// request performs one protocol round trip, playing the role of the C shim.
func request(t *testing.T, sock, line string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
		t.Fatalf("send %q: %v", line, err)
	}
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read reply to %q: %v", line, err)
	}
	return resp[:len(resp)-1]
}

// abs maps a manifest-relative path into the workspace.
func (e *env) abs(rel string) string {
	return filepath.Join(e.root, filepath.FromSlash(rel))
}

// blocksOf returns the file's allocated block count (0 = fully sparse).
func blocksOf(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no syscall.Stat_t on this platform")
	}
	return st.Blocks
}
