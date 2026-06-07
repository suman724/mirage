// Package cache is a server-side in-memory chunk cache that fronts a slower
// backing store (the channelstore). It implements chunk.Store and wraps another
// chunk.Store, mirroring desync's cache-store chaining (design §4.1, §5): a
// cache hit is served locally; a miss falls through to the channel, then the
// fetched chunk is cached so re-reads — and reads of duplicate chunks shared by
// multiple files — are free.
//
// Concurrent faults for the SAME hash are collapsed with single-flight: only
// one channel fetch happens even if many readers request the chunk at once.
// This matters because desync's assembler and (later) concurrent FUSE reads fan
// out many overlapping GetChunk calls.
package cache

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/singleflight"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/logging"
)

// Store caches chunk bytes in memory in front of a backing chunk.Store.
type Store struct {
	backing chunk.Store
	log     *slog.Logger

	mu     sync.RWMutex
	chunks map[chunk.Hash][]byte
	group  singleflight.Group

	hits   atomic.Uint64
	misses atomic.Uint64
}

// New returns a cache fronting backing. logger may be nil.
func New(backing chunk.Store, logger *slog.Logger) *Store {
	return &Store{
		backing: backing,
		log:     logging.OrDefault(logger),
		chunks:  make(map[chunk.Hash][]byte),
	}
}

// Stats reports cumulative cache hits and misses.
func (s *Store) Stats() (hits, misses uint64) {
	return s.hits.Load(), s.misses.Load()
}

// Len returns the number of distinct chunks currently cached.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.chunks)
}

// GetChunk returns the chunk bytes for h, serving from cache when possible and
// otherwise faulting through the backing store exactly once per hash.
func (s *Store) GetChunk(ctx context.Context, h chunk.Hash) ([]byte, error) {
	if data, ok := s.lookup(h); ok {
		s.hits.Add(1)
		s.log.Debug("cache hit", "hash", h.String())
		return data, nil
	}

	// Collapse concurrent misses for the same hash into one backing fetch.
	v, err, shared := s.group.Do(h.String(), func() (any, error) {
		// Re-check: another flight may have populated the cache between the
		// initial lookup and acquiring the single-flight slot.
		if data, ok := s.lookup(h); ok {
			return data, nil
		}
		data, err := s.backing.GetChunk(ctx, h)
		if err != nil {
			return nil, err
		}
		s.store(h, data)
		s.misses.Add(1)
		s.log.Debug("cache miss; fetched from backing", "hash", h.String(), "bytes", len(data))
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	if shared {
		s.log.Debug("cache miss coalesced via single-flight", "hash", h.String())
	}
	return v.([]byte), nil
}

func (s *Store) lookup(h chunk.Hash) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.chunks[h]
	return data, ok
}

func (s *Store) store(h chunk.Hash, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks[h] = data
}
