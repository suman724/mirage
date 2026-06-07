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
// reply.
package channelstore

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/suman724/mirage/internal/chunk"
	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
)

// SendFunc sends a ServerFrame up the stream. It must be safe for the store to
// call; the transport serializes concurrent sends.
type SendFunc func(*miragev1.ServerFrame) error

// Store fetches chunks over the channel. It implements chunk.Store.
type Store struct {
	send SendFunc

	nextID  atomic.Uint64
	reqs    atomic.Uint64 // total ChunkRequests originated (metric)
	mu      sync.Mutex
	pending map[uint64]chan *miragev1.ChunkResponse
}

// New returns a Store that originates ChunkRequests via send.
func New(send SendFunc) *Store {
	return &Store{
		send:    send,
		pending: make(map[uint64]chan *miragev1.ChunkResponse),
	}
}

// Requests returns how many ChunkRequests this store has originated. Used to
// prove the server obtained data only via the channel.
func (s *Store) Requests() uint64 { return s.reqs.Load() }

// GetChunk sends a ChunkRequest for h and waits for the matching response.
func (s *Store) GetChunk(ctx context.Context, h chunk.Hash) ([]byte, error) {
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
	frame := &miragev1.ServerFrame{
		Payload: &miragev1.ServerFrame_ChunkRequest{
			ChunkRequest: &miragev1.ChunkRequest{
				RequestId:   id,
				ChunkHashes: [][]byte{h[:]},
			},
		},
	}
	if err := s.send(frame); err != nil {
		return nil, fmt.Errorf("channelstore: send ChunkRequest: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.GetError() != "" {
			return nil, fmt.Errorf("channelstore: client rejected chunk %s: %s", h, resp.GetError())
		}
		for _, c := range resp.GetChunks() {
			got, err := chunk.HashFromBytes(c.GetHash())
			if err != nil {
				return nil, err
			}
			if got != h {
				continue
			}
			data := c.GetData()
			// Verify the client served the bytes we actually asked for.
			if chunk.HashOf(data) != h {
				return nil, fmt.Errorf("channelstore: chunk %s failed hash verification", h)
			}
			return data, nil
		}
		return nil, fmt.Errorf("channelstore: response %d missing chunk %s", id, h)
	}
}

// Dispatch routes a ChunkResponse to the GetChunk call waiting on its
// request_id. Unknown ids are dropped.
func (s *Store) Dispatch(resp *miragev1.ChunkResponse) {
	s.mu.Lock()
	ch := s.pending[resp.GetRequestId()]
	s.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
}
