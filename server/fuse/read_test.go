package fuse

import (
	"bytes"
	"sync/atomic"
	"testing"

	"github.com/folbricht/desync"

	"github.com/suman724/mirage/internal/chunk"
)

// memStore is an in-memory desync.Store built from a hash->bytes map, standing
// in for the cache->channel chain so ReadRange can be tested without a mount.
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

// buildFile chunks data the way the client does and returns its index + a store
// holding the chunks.
func buildFile(t *testing.T, data []byte) (desync.Index, *memStore) {
	t.Helper()
	refs, chunks, err := chunk.Split(data)
	if err != nil {
		t.Fatal(err)
	}
	store := &memStore{chunks: make(map[desync.ChunkID][]byte)}
	for h, b := range chunks {
		store.chunks[desync.ChunkID(h)] = b
	}
	return IndexFromRefs(refs), store
}

func TestReadRangeFullFile(t *testing.T) {
	data := make([]byte, 500*1024) // multi-chunk
	for i := range data {
		data[i] = byte(i * 7)
	}
	idx, store := buildFile(t, data)

	if got := FileSize(idx); got != int64(len(data)) {
		t.Fatalf("FileSize=%d, want %d", got, len(data))
	}

	dest := make([]byte, len(data))
	n, err := ReadRange(idx, store, dest, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) || !bytes.Equal(dest, data) {
		t.Fatalf("full read mismatch: n=%d", n)
	}
}

func TestReadRangeArbitraryWindows(t *testing.T) {
	data := make([]byte, 300*1024)
	for i := range data {
		data[i] = byte(i*131 + 17)
	}
	idx, store := buildFile(t, data)

	// Read many offset/length windows, including ones that straddle chunk
	// boundaries and run past EOF.
	windows := []struct{ off, length int }{
		{0, 1}, {0, 100}, {1, 100}, {1023, 5000}, {65536, 1}, {65535, 3},
		{len(data) - 10, 10}, {len(data) - 10, 50}, {len(data), 16}, {123456, 70000},
	}
	for _, w := range windows {
		dest := make([]byte, w.length)
		n, err := ReadRange(idx, store, dest, int64(w.off))
		if err != nil {
			t.Fatalf("off=%d len=%d: %v", w.off, w.length, err)
		}
		want := data[min(w.off, len(data)):min(w.off+w.length, len(data))]
		if !bytes.Equal(dest[:n], want) {
			t.Fatalf("off=%d len=%d: got %d bytes, mismatch", w.off, w.length, n)
		}
	}
}

func TestReadRangeFaultsOnlyOverlappingChunks(t *testing.T) {
	// 700 KiB guarantees >= 3 chunks (max chunk size is 256 KiB).
	data := make([]byte, 700*1024)
	for i := range data {
		data[i] = byte(i*73 + i/512)
	}
	idx, store := buildFile(t, data)
	if len(idx.Chunks) < 3 {
		t.Fatalf("expected >= 3 chunks for 700 KiB, got %d", len(idx.Chunks))
	}

	// Read only the first byte: must fault just the first chunk, not the tree.
	dest := make([]byte, 1)
	if _, err := ReadRange(idx, store, dest, 0); err != nil {
		t.Fatal(err)
	}
	if store.gets.Load() != 1 {
		t.Fatalf("reading 1 byte faulted %d chunks, want 1 (lazy)", store.gets.Load())
	}
}

func TestReadRangeNegativeOffset(t *testing.T) {
	idx, store := buildFile(t, []byte("hello"))
	if _, err := ReadRange(idx, store, make([]byte, 4), -1); err == nil {
		t.Fatal("expected error for negative offset")
	}
}
