// Package chunk holds the content-addressed chunking primitives shared by the
// Mirage client and server: the hash type, the per-tree manifest, the
// content-defined chunker, and the Store seam.
//
// Chunking and content hashing are backed by folbricht/desync: Split uses
// desync's content-defined chunker (CDC), so identical content anywhere in the
// tree — or across files — collapses to the same chunk, and chunk IDs are
// desync chunk IDs (SHA-512/256, desync's default digest). This is the real
// chunker, not a placeholder.
//
// The Store seam (GetChunk(hash) -> bytes) is the integration boundary between
// "lazy filesystem" and "outbound-only socket": the server's channelstore
// implements it over the gRPC stream, the client's chunkstore implements it
// from local memory. A desync-native store can be adapted to this shape too.
package chunk

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/folbricht/desync"
)

// Content-defined chunking parameters (bytes). These follow casync/desync
// conventions: a 64 KiB average with a 4x window on either side. Tuning these
// trades fault count against dedup ratio (design §8, open question 2).
const (
	ChunkMin = 16 * 1024  // 16 KiB
	ChunkAvg = 64 * 1024  // 64 KiB
	ChunkMax = 256 * 1024 // 256 KiB
)

// Hash is a content hash identifying a chunk: a desync chunk ID (SHA-512/256).
// It marshals to/from hex in JSON so manifests are human-readable.
type Hash [32]byte

// String returns the hex encoding of the hash.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// MarshalText implements encoding.TextMarshaler (hex).
func (h Hash) MarshalText() ([]byte, error) {
	return []byte(hex.EncodeToString(h[:])), nil
}

// UnmarshalText implements encoding.TextUnmarshaler (hex).
func (h *Hash) UnmarshalText(text []byte) error {
	b, err := hex.DecodeString(string(text))
	if err != nil {
		return err
	}
	if len(b) != len(h) {
		return fmt.Errorf("chunk: bad hash length %d, want %d", len(b), len(h))
	}
	copy(h[:], b)
	return nil
}

// HashOf computes the content hash of a chunk's bytes using desync's digest, so
// it matches the chunk IDs produced by Split.
func HashOf(data []byte) Hash {
	return Hash(desync.Digest.Sum(data))
}

// HashFromBytes converts a raw wire hash (proto bytes) to a Hash.
func HashFromBytes(b []byte) (Hash, error) {
	var h Hash
	if len(b) != len(h) {
		return h, fmt.Errorf("chunk: bad hash length %d, want %d", len(b), len(h))
	}
	copy(h[:], b)
	return h, nil
}

// Ref is one chunk's identity and size within a file.
type Ref struct {
	Hash Hash   `json:"hash"`
	Size uint32 `json:"size"`
}

// FileEntry is the ordered list of chunks that reconstruct one file, keyed by
// its path relative to the workspace root.
type FileEntry struct {
	Path   string `json:"path"` // slash-separated, relative to workspace root
	Mode   uint32 `json:"mode"` // unix file mode bits
	Chunks []Ref  `json:"chunks"`
}

// Manifest is the published index: which files exist and which chunks compose
// them. It is tiny relative to the tree and is sent up front; chunk *contents*
// are faulted lazily afterward.
type Manifest struct {
	Files []FileEntry `json:"files"`
}

// Marshal serializes the manifest for IndexPublish.caidx.
func (m *Manifest) Marshal() ([]byte, error) { return json.Marshal(m) }

// Unmarshal parses a manifest from IndexPublish.caidx.
func Unmarshal(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// TotalChunks counts chunk references across all files (with duplicates).
func (m *Manifest) TotalChunks() uint32 {
	var n uint32
	for _, f := range m.Files {
		n += uint32(len(f.Chunks))
	}
	return n
}

// TotalBytes is the logical size of the materialized tree.
func (m *Manifest) TotalBytes() uint64 {
	var n uint64
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			n += uint64(c.Size)
		}
	}
	return n
}

// UniqueHashes returns the set of distinct chunk hashes in the manifest.
func (m *Manifest) UniqueHashes() map[Hash]struct{} {
	set := make(map[Hash]struct{})
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			set[c.Hash] = struct{}{}
		}
	}
	return set
}

// Split applies desync's content-defined chunker to data, returning the ordered
// chunk refs and a map of hash -> bytes for the distinct chunks. Concatenating
// the chunk bytes in ref order reproduces the input exactly.
func Split(data []byte) ([]Ref, map[Hash][]byte, error) {
	c, err := desync.NewChunker(bytes.NewReader(data), ChunkMin, ChunkAvg, ChunkMax)
	if err != nil {
		return nil, nil, fmt.Errorf("chunk: new chunker: %w", err)
	}
	refs := make([]Ref, 0, len(data)/ChunkAvg+1)
	chunks := make(map[Hash][]byte)
	for {
		_, buf, err := c.Next()
		if err != nil {
			return nil, nil, fmt.Errorf("chunk: split: %w", err)
		}
		if len(buf) == 0 {
			break // end of stream
		}
		// desync reuses its internal buffer across Next calls; copy so the
		// chunk bytes stay valid after the next iteration.
		b := make([]byte, len(buf))
		copy(b, buf)
		h := HashOf(b)
		refs = append(refs, Ref{Hash: h, Size: uint32(len(b))})
		if _, ok := chunks[h]; !ok {
			chunks[h] = b
		}
	}
	return refs, chunks, nil
}

// Store is the integration seam: fetch a chunk's bytes by content hash. The
// server's channelstore implements this over the gRPC stream; the client's
// chunkstore implements it from local memory.
type Store interface {
	GetChunk(ctx context.Context, h Hash) ([]byte, error)
}
