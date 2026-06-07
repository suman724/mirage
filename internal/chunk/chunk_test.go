package chunk

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSplitDeterministicAndDedup(t *testing.T) {
	data := bytes.Repeat([]byte("abcd"), ChunkSize) // > 1 chunk, with repetition

	refs1, chunks1 := Split(data)
	refs2, _ := Split(data)

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
		got = append(got, b...)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("reassembled bytes != original")
	}
}

func TestSplitDedupIdenticalChunks(t *testing.T) {
	// Two full chunks of identical content -> one distinct chunk, two refs.
	block := bytes.Repeat([]byte("x"), ChunkSize)
	data := append(append([]byte{}, block...), block...)

	refs, chunks := Split(data)
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %d", len(refs))
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 distinct chunk after dedup, got %d", len(chunks))
	}
	if refs[0].Hash != refs[1].Hash {
		t.Fatalf("identical content produced different hashes")
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
	refsA, _ := Split([]byte("file A content"))
	refsB, _ := Split([]byte("file B content, a bit longer"))
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
