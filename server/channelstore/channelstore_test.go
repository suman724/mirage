package channelstore

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/folbricht/desync"

	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
)

// chunkID returns the desync chunk ID for data (matches what the client stores).
func chunkID(data []byte) desync.ChunkID { return desync.Digest.Sum(data) }

func TestGetChunkRoundTrip(t *testing.T) {
	data := []byte("the chunk bytes")
	id := chunkID(data)

	sent := make(chan *miragev1.ChunkRequest, 1)
	s := New(context.Background(), func(f *miragev1.ServerFrame) error {
		sent <- f.GetChunkRequest()
		return nil
	}, nil)

	type res struct {
		data []byte
		err  error
	}
	done := make(chan res, 1)
	go func() {
		ck, err := s.GetChunk(id)
		if err != nil {
			done <- res{nil, err}
			return
		}
		b, err := ck.Data()
		done <- res{b, err}
	}()

	req := <-sent
	if s.Requests() != 1 {
		t.Fatalf("Requests()=%d, want 1", s.Requests())
	}
	s.Dispatch(&miragev1.ChunkResponse{
		RequestId: req.GetRequestId(),
		Chunks:    []*miragev1.Chunk{{Hash: id[:], Data: data}},
	})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if string(r.data) != string(data) {
			t.Fatalf("got %q, want %q", r.data, data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetChunk timed out")
	}
}

func TestGetChunkRejection(t *testing.T) {
	id := chunkID([]byte("secret"))
	sent := make(chan *miragev1.ChunkRequest, 1)
	s := New(context.Background(), func(f *miragev1.ServerFrame) error {
		sent <- f.GetChunkRequest()
		return nil
	}, nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := s.GetChunk(id)
		errCh <- err
	}()

	req := <-sent
	s.Dispatch(&miragev1.ChunkResponse{RequestId: req.GetRequestId(), Error: "hash not in published index"})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error when client rejects the chunk")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetChunk timed out")
	}
}

func TestGetChunkHashVerification(t *testing.T) {
	id := chunkID([]byte("expected"))
	sent := make(chan *miragev1.ChunkRequest, 1)
	s := New(context.Background(), func(f *miragev1.ServerFrame) error {
		sent <- f.GetChunkRequest()
		return nil
	}, nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := s.GetChunk(id)
		errCh <- err
	}()

	req := <-sent
	// Server claims id but sends tampered bytes -> desync NewChunkWithID must
	// fail verification.
	s.Dispatch(&miragev1.ChunkResponse{
		RequestId: req.GetRequestId(),
		Chunks:    []*miragev1.Chunk{{Hash: id[:], Data: []byte("tampered")}},
	})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected hash-verification error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetChunk timed out")
	}
}

// TestGetChunkConcurrent proves that many concurrent GetChunk calls multiplex
// correctly over the single stream: each caller gets the bytes for its own
// chunk, never another's, even when responses arrive interleaved and out of
// order. This is the property desync's concurrent assembler (and concurrent
// FUSE reads) rely on.
func TestGetChunkConcurrent(t *testing.T) {
	const n = 200
	want := make(map[desync.ChunkID][]byte, n)
	for i := 0; i < n; i++ {
		data := []byte("chunk-number-" + strconv.Itoa(i))
		want[chunkID(data)] = data
	}

	requests := make(chan *miragev1.ChunkRequest, n)
	s := New(context.Background(), func(f *miragev1.ServerFrame) error {
		requests <- f.GetChunkRequest()
		return nil
	}, nil)

	// Responder: acts as the client, answering each request by hash. Replies
	// run in their own goroutines to interleave/reorder responses on purpose.
	go func() {
		for req := range requests {
			req := req
			go func() {
				var id desync.ChunkID
				copy(id[:], req.GetChunkHashes()[0])
				s.Dispatch(&miragev1.ChunkResponse{
					RequestId: req.GetRequestId(),
					Chunks:    []*miragev1.Chunk{{Hash: id[:], Data: want[id]}},
				})
			}()
		}
	}()

	var wg sync.WaitGroup
	var fail atomic.Bool
	for id, data := range want {
		wg.Add(1)
		go func(id desync.ChunkID, data []byte) {
			defer wg.Done()
			ck, err := s.GetChunk(id)
			if err != nil {
				fail.Store(true)
				t.Errorf("chunk %s: %v", id, err)
				return
			}
			got, err := ck.Data()
			if err != nil || string(got) != string(data) {
				fail.Store(true)
				t.Errorf("chunk %s: got %q err %v, want %q", id, got, err, data)
			}
		}(id, data)
	}
	wg.Wait()
	close(requests)

	if fail.Load() {
		t.Fatal("concurrent GetChunk returned mismatched data")
	}
	if s.Requests() != uint64(n) {
		t.Fatalf("expected %d requests, got %d", n, s.Requests())
	}
}

func TestGetChunkContextCancel(t *testing.T) {
	id := chunkID([]byte("never answered"))
	// A store whose stream context is already cancelled: the fetch must fail
	// promptly rather than block on a response that will never come.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, func(f *miragev1.ServerFrame) error { return nil }, nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := s.GetChunk(id)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected context cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetChunk did not honor context cancellation")
	}
}
