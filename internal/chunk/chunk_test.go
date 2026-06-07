package chunk

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSplitDeterministicAndReassembles(t *testing.T) {
	// ~1 MiB of varied content so CDC produces several chunks.
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i*31 + i/7)
	}

	refs1, chunks1, err := Split(data)
	if err != nil {
		t.Fatal(err)
	}
	refs2, _, err := Split(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs1) < 2 {
		t.Fatalf("expected multiple chunks for 1 MiB input, got %d", len(refs1))
	}

	// Chunking must be deterministic.
	if len(refs1) != len(refs2) {
		t.Fatalf("non-deterministic ref count: %d vs %d", len(refs1), len(refs2))
	}
	for i := range refs1 {
		if refs1[i] != refs2[i] {
			t.Fatalf("ref %d differs between runs", i)
		}
	}

	// Reassembling the distinct chunks in ref order must reproduce the input.
	var got []byte
	for _, r := range refs1 {
		b, ok := chunks1[r.Hash]
		if !ok {
			t.Fatalf("ref hash %s missing from chunk map", r.Hash)
		}
		if uint32(len(b)) != r.Size {
			t.Fatalf("chunk size %d != ref size %d", len(b), r.Size)
		}
		got = append(got, b...)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("reassembled bytes != original")
	}
}

func TestSplitDedupsRepeatedContent(t *testing.T) {
	// Highly repetitive content must dedup: many refs collapse to far fewer
	// distinct chunks. (CDC cuts repeated regions identically.)
	data := bytes.Repeat([]byte("mirage"), 1<<18) // ~1.5 MiB

	refs, chunks, err := Split(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(refs))
	}
	if len(chunks) >= len(refs) {
		t.Fatalf("expected dedup (distinct %d < refs %d)", len(chunks), len(refs))
	}

	// Still reassembles exactly.
	var got []byte
	for _, r := range refs {
		got = append(got, chunks[r.Hash]...)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("reassembled bytes != original")
	}
}

func TestSplitEmpty(t *testing.T) {
	refs, chunks, err := Split(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 || len(chunks) != 0 {
		t.Fatalf("empty input should yield no chunks, got %d refs / %d chunks", len(refs), len(chunks))
	}
}

func TestHashTextRoundTrip(t *testing.T) {
	h := HashOf([]byte("hello"))
	text, err := h.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	var back Hash
	if err := back.UnmarshalText(text); err != nil {
		t.Fatal(err)
	}
	if back != h {
		t.Fatalf("hash round-trip mismatch")
	}
}

func TestHashFromBytes(t *testing.T) {
	h := HashOf([]byte("data"))
	got, err := HashFromBytes(h[:])
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("HashFromBytes mismatch")
	}
	if _, err := HashFromBytes([]byte{1, 2, 3}); err == nil {
		t.Fatalf("expected error for short hash")
	}
}

func TestManifestRoundTripAndTotals(t *testing.T) {
	refsA, _, err := Split([]byte("file A content"))
	if err != nil {
		t.Fatal(err)
	}
	refsB, _, err := Split([]byte("file B content, a bit longer"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Files: []FileEntry{
		{Path: "a.txt", Mode: 0o644, Chunks: refsA},
		{Path: "dir/b.txt", Mode: 0o644, Chunks: refsB},
	}}

	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: hashes serialize as hex strings, not number arrays.
	if !json.Valid(raw) {
		t.Fatalf("manifest not valid json")
	}

	back, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.TotalChunks() != m.TotalChunks() {
		t.Fatalf("TotalChunks mismatch: %d vs %d", back.TotalChunks(), m.TotalChunks())
	}
	if back.TotalBytes() != m.TotalBytes() {
		t.Fatalf("TotalBytes mismatch: %d vs %d", back.TotalBytes(), m.TotalBytes())
	}
	if len(back.UniqueHashes()) != len(m.UniqueHashes()) {
		t.Fatalf("UniqueHashes mismatch")
	}
}
