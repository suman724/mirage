// Package chunkstore is the client-side store of chunk bytes keyed by content
// hash. It is built from the workspace index at publish time and answers the
// server's ChunkRequests.
//
// SECURITY (design §6): the store contains ONLY the chunks of files that the
// indexer published. A hash that is not present — including any chunk of an
// excluded/secret file, which was never chunked — is rejected, not served.
// This is the client-side enforcement of the chunk-hash protocol: the server
// can only obtain hashes the client published.
package chunkstore

import (
	"github.com/suman724/mirage/internal/chunk"
)

// Store holds published chunk bytes by hash.
type Store struct {
	chunks map[chunk.Hash][]byte
}

// New returns an empty store.
func New() *Store {
	return &Store{chunks: make(map[chunk.Hash][]byte)}
}

// Put adds a chunk's bytes under its hash.
func (s *Store) Put(h chunk.Hash, data []byte) {
	s.chunks[h] = data
}

// Has reports whether the hash was published.
func (s *Store) Has(h chunk.Hash) bool {
	_, ok := s.chunks[h]
	return ok
}

// Get returns the bytes for a published hash. found is false for any hash not
// in the published index — the caller MUST reject (not serve) such a request.
func (s *Store) Get(h chunk.Hash) (data []byte, found bool) {
	data, found = s.chunks[h]
	return
}

// Len returns the number of distinct chunks held.
func (s *Store) Len() int { return len(s.chunks) }
