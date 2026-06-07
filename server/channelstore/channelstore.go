// Package channelstore implements desync.Store on the SERVER side by fetching
// chunk bytes over the already-open gRPC stream that the client dialed.
//
// This is the core bridge described in design §4.1: a Store whose GetChunk
// does not hit S3/HTTP but sends a ChunkRequest *down* the client-initiated
// connection and awaits the matching ChunkResponse. It is the single place
// where "lazy filesystem" meets "outbound-only socket". The server never
// dials; it only originates requests over the open stream.
//
// It implements desync.Store (not a bespoke interface) so the rest of the
// server can reuse desync's machinery — Cache, DedupQueue, AssembleFile, the
// FUSE read path — instead of re-implementing it. desync's Store signature has
// no context.Context, so cancellation and per-fetch timeout are handled
// internally from the stream context (the same approach desync's own HTTP
// store takes). Returning a desync *Chunk via NewChunkWithID also gives us
// hash verification for free.
//
// Requests are correlated to responses by request_id. The transport feeds
// incoming ChunkResponses to Dispatch; GetChunk blocks on the matching reply.
// GetChunk is safe for concurrent use — many overlapping faults multiplex over
// the single stream via request_id correlation.
package channelstore

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/folbricht/desync"

	"github.com/suman724/mirage/internal/logging"
	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
)

// DefaultFetchTimeout bounds how long a single chunk fetch may wait for the
// client to answer before the request is abandoned.
const DefaultFetchTimeout = 30 * time.Second

// SendFunc sends a ServerFrame up the stream. It must be safe for the store to
// call; the transport serializes concurrent sends.
type SendFunc func(*miragev1.ServerFrame) error

// Store fetches chunks over the channel. It implements desync.Store.
type Store struct {
	ctx          context.Context // stream context; cancels in-flight fetches on disconnect
	send         SendFunc
	log          *slog.Logger
	fetchTimeout time.Duration

	nextID  atomic.Uint64
	reqs    atomic.Uint64 // total ChunkRequests originated (metric)
	mu      sync.Mutex
	pending map[uint64]chan *miragev1.ChunkResponse
}

var _ desync.Store = (*Store)(nil)

// New returns a Store that originates ChunkRequests via send. ctx is the stream
// context: when it is cancelled (the connection drops), in-flight fetches fail.
// logger may be nil (defaults to slog.Default()).
func New(ctx context.Context, send SendFunc, logger *slog.Logger) *Store {
	return &Store{
		ctx:          ctx,
		send:         send,
		log:          logging.OrDefault(logger),
		fetchTimeout: DefaultFetchTimeout,
		pending:      make(map[uint64]chan *miragev1.ChunkResponse),
	}
}

// Requests returns how many ChunkRequests this store has originated. With a
// cache/dedup layer in front, this equals the number of distinct chunks
// actually fetched over the wire. Used to prove the server obtained data only
// via the channel.
func (s *Store) Requests() uint64 { return s.reqs.Load() }

// GetChunk sends a ChunkRequest for id and waits for the matching response,
// bounded by the store's fetch timeout and the stream context. It implements
// desync.Store.
func (s *Store) GetChunk(id desync.ChunkID) (*desync.Chunk, error) {
	ctx, cancel := context.WithTimeout(s.ctx, s.fetchTimeout)
	defer cancel()

	reqID := s.nextID.Add(1)
	ch := make(chan *miragev1.ChunkResponse, 1)

	s.mu.Lock()
	s.pending[reqID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, reqID)
		s.mu.Unlock()
	}()

	s.reqs.Add(1)
	s.log.Debug("requesting chunk over channel", "request_id", reqID, "chunk", id.String())
	frame := &miragev1.ServerFrame{
		Payload: &miragev1.ServerFrame_ChunkRequest{
			ChunkRequest: &miragev1.ChunkRequest{
				RequestId:   reqID,
				ChunkHashes: [][]byte{id[:]},
			},
		},
	}
	if err := s.send(frame); err != nil {
		return nil, fmt.Errorf("channelstore: send ChunkRequest %d for %s: %w", reqID, id, err)
	}

	select {
	case <-ctx.Done():
		s.log.Warn("chunk fetch aborted", "request_id", reqID, "chunk", id.String(), "err", ctx.Err())
		return nil, fmt.Errorf("channelstore: fetch chunk %s (request %d): %w", id, reqID, ctx.Err())
	case resp := <-ch:
		if resp.GetError() != "" {
			return nil, fmt.Errorf("channelstore: client rejected chunk %s (request %d): %s", id, reqID, resp.GetError())
		}
		for _, c := range resp.GetChunks() {
			if !chunkIDEqual(c.GetHash(), id) {
				continue
			}
			// NewChunkWithID verifies the bytes hash to id (skipVerify=false).
			ck, err := desync.NewChunkWithID(id, c.GetData(), false)
			if err != nil {
				return nil, fmt.Errorf("channelstore: chunk %s (request %d): %w", id, reqID, err)
			}
			s.log.Debug("received chunk over channel", "request_id", reqID, "chunk", id.String(), "bytes", len(c.GetData()))
			return ck, nil
		}
		return nil, fmt.Errorf("channelstore: response %d missing requested chunk %s", reqID, id)
	}
}

// HasChunk reports whether the store can serve id. The server only ever
// requests hashes present in the published index, so this is always true; the
// real existence/authorization check is enforced on the client, which rejects
// unpublished hashes. It implements desync.Store.
func (s *Store) HasChunk(id desync.ChunkID) (bool, error) { return true, nil }

// Close implements desync.Store. The stream lifecycle is owned by the
// transport, so there is nothing to release here.
func (s *Store) Close() error { return nil }

// String implements desync.Store (fmt.Stringer).
func (s *Store) String() string { return "mirage-channelstore" }

// Dispatch routes a ChunkResponse to the GetChunk call waiting on its
// request_id. Unknown ids are dropped (logged at debug) — they can occur if a
// fetch already timed out before the response arrived.
func (s *Store) Dispatch(resp *miragev1.ChunkResponse) {
	s.mu.Lock()
	ch := s.pending[resp.GetRequestId()]
	s.mu.Unlock()
	if ch != nil {
		ch <- resp
		return
	}
	s.log.Debug("dropping response for unknown/expired request", "request_id", resp.GetRequestId())
}

// chunkIDEqual reports whether a raw wire hash equals a desync ChunkID.
func chunkIDEqual(raw []byte, id desync.ChunkID) bool {
	if len(raw) != len(id) {
		return false
	}
	for i := range id {
		if raw[i] != id[i] {
			return false
		}
	}
	return true
}
