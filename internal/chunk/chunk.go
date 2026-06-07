// Package chunk holds the content-addressed chunking primitives shared by the
// Mirage client and server: the hash type, the per-tree manifest (our stand-in
// for a desync .caidx), a trivial fixed-size SHA-256 chunker, and the Store
// seam.
//
// PLACEHOLDER NOTE: the chunker here is a fixed-size SHA-256 splitter, not
// desync's content-defined chunking. It exists so the first end-to-end spike
// does not block on desync integration. The important part is the Store seam
// (GetChunk(hash) -> bytes): swapping in folbricht/desync later means
// implementing Store with desync's chunker + index and is a local change that
// does not touch the transport or reconstruction code.
package chunk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ChunkSize is the fixed chunk size for the placeholder chunker (64 KiB).
const ChunkSize = 64 * 1024

// Hash is a content hash (SHA-256) identifying a chunk. It marshals to/from hex
// in JSON so manifests are human-readable.
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

// HashOf computes the content hash of a chunk's bytes.
func HashOf(data []byte) Hash {
	return Hash(sha256.Sum256(data))
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
// them. It is the analogue of a desync directory index (.caidx). It is tiny
// relative to the tree and is sent up front; chunk *contents* are faulted
// lazily afterward.
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

// Split applies the placeholder fixed-size chunker to data, returning the
// ordered chunk refs and a map of hash -> bytes for the distinct chunks.
func Split(data []byte) ([]Ref, map[Hash][]byte) {
	refs := make([]Ref, 0, len(data)/ChunkSize+1)
	chunks := make(map[Hash][]byte)
	for off := 0; off < len(data); off += ChunkSize {
		end := off + ChunkSize
		if end > len(data) {
			end = len(data)
		}
		// Copy so callers can mutate the source buffer safely.
		buf := make([]byte, end-off)
		copy(buf, data[off:end])
		h := HashOf(buf)
		refs = append(refs, Ref{Hash: h, Size: uint32(len(buf))})
		if _, ok := chunks[h]; !ok {
			chunks[h] = buf
		}
	}
	return refs, chunks
}

// Store is the integration seam: fetch a chunk's bytes by content hash. The
// server's channelstore implements this over the gRPC stream; the client's
// chunkstore implements it from local memory. desync's own Store can be
// adapted to this shape later.
type Store interface {
	GetChunk(ctx context.Context, h Hash) ([]byte, error)
}
