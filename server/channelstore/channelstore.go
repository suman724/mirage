// Package channelstore implements chunk.Store on the SERVER side by fetching
// chunk bytes over the already-open gRPC stream that the client dialed.
//
// This is the core bridge described in design §4.1: a Store whose GetChunk
// does not hit S3/HTTP but sends a ChunkRequest *down* the client-initiated
// connection and awaits the matching ChunkResponse. It is the single place
// where "lazy filesystem" meets "outbound-only socket". The server never
// dials; it only originates requests over the open stream.
//
// Requests are correlated to responses by request_id. The transport layer
// feeds incoming ChunkResponses to Dispatch; GetChunk blocks on the matching
// reply. GetChunk is safe for concurrent use — desync's assembler (and, later,
// concurrent FUSE reads) fan out many GetChunk calls that all multiplex over
// the single stream via request_id correlation.
package channelstore

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/logging"
	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
)

// DefaultFetchTimeout bounds how long a single chunk fetch may wait for the
// client to answer before the request is abandoned.
const DefaultFetchTimeout = 30 * time.Second

// SendFunc sends a ServerFrame up the stream. It must be safe for the store to
// call; the transport serializes concurrent sends.
type SendFunc func(*miragev1.ServerFrame) error

// Store fetches chunks over the channel. It implements chunk.Store.
type Store struct {
	send         SendFunc
	log          *slog.Logger
	fetchTimeout time.Duration

	nextID  atomic.Uint64
	reqs    atomic.Uint64 // total ChunkRequests originated (metric)
	mu      sync.Mutex
	pending map[uint64]chan *miragev1.ChunkResponse
}

// New returns a Store that originates ChunkRequests via send. logger may be nil
// (defaults to slog.Default()).
func New(send SendFunc, logger *slog.Logger) *Store {
	return &Store{
		send:         send,
		log:          logging.OrDefault(logger),
		fetchTimeout: DefaultFetchTimeout,
		pending:      make(map[uint64]chan *miragev1.ChunkResponse),
	}
}

// Requests returns how many ChunkRequests this store has originated. Used to
// prove the server obtained data only via the channel.
func (s *Store) Requests() uint64 { return s.reqs.Load() }

// GetChunk sends a ChunkRequest for h and waits for the matching response,
// bounded by the store's fetch timeout and the caller's context.
func (s *Store) GetChunk(ctx context.Context, h chunk.Hash) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()

	id := s.nextID.Add(1)
	ch := make(chan *miragev1.ChunkResponse, 1)

	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	s.reqs.Add(1)
	s.log.Debug("requesting chunk over channel", "request_id", id, "hash", h.String())
	frame := &miragev1.ServerFrame{
		Payload: &miragev1.ServerFrame_ChunkRequest{
			ChunkRequest: &miragev1.ChunkRequest{
				RequestId:   id,
				ChunkHashes: [][]byte{h[:]},
			},
		},
	}
	if err := s.send(frame); err != nil {
		return nil, fmt.Errorf("channelstore: send ChunkRequest %d for %s: %w", id, h, err)
	}

	select {
	case <-ctx.Done():
		s.log.Warn("chunk fetch aborted", "request_id", id, "hash", h.String(), "err", ctx.Err())
		return nil, fmt.Errorf("channelstore: fetch chunk %s (request %d): %w", h, id, ctx.Err())
	case resp := <-ch:
		if resp.GetError() != "" {
			return nil, fmt.Errorf("channelstore: client rejected chunk %s (request %d): %s", h, id, resp.GetError())
		}
		for _, c := range resp.GetChunks() {
			got, err := chunk.HashFromBytes(c.GetHash())
			if err != nil {
				return nil, fmt.Errorf("channelstore: response %d: %w", id, err)
			}
			if got != h {
				continue
			}
			data := c.GetData()
			// Verify the client served the bytes we actually asked for.
			if chunk.HashOf(data) != h {
				return nil, fmt.Errorf("channelstore: chunk %s (request %d) failed hash verification", h, id)
			}
			s.log.Debug("received chunk over channel", "request_id", id, "hash", h.String(), "bytes", len(data))
			return data, nil
		}
		return nil, fmt.Errorf("channelstore: response %d missing requested chunk %s", id, h)
	}
}

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
