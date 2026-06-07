package channelstore

import (
	"context"
	"testing"
	"time"

	"github.com/suman724/mirage/internal/chunk"
	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
)

// drive runs GetChunk concurrently, capturing the outgoing ChunkRequest so the
// test can craft a matching response.
func TestGetChunkRoundTrip(t *testing.T) {
	data := []byte("the chunk bytes")
	h := chunk.HashOf(data)

	sent := make(chan *miragev1.ChunkRequest, 1)
	var s *Store
	s = New(func(f *miragev1.ServerFrame) error {
		sent <- f.GetChunkRequest()
		return nil
	})

	type res struct {
		data []byte
		err  error
	}
	done := make(chan res, 1)
	go func() {
		b, err := s.GetChunk(context.Background(), h)
		done <- res{b, err}
	}()

	req := <-sent
	if s.Requests() != 1 {
		t.Fatalf("Requests()=%d, want 1", s.Requests())
	}
	s.Dispatch(&miragev1.ChunkResponse{
		RequestId: req.GetRequestId(),
		Chunks:    []*miragev1.Chunk{{Hash: h[:], Data: data}},
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
	h := chunk.HashOf([]byte("secret"))
	sent := make(chan *miragev1.ChunkRequest, 1)
	s := New(func(f *miragev1.ServerFrame) error {
		sent <- f.GetChunkRequest()
		return nil
	})

	errCh := make(chan error, 1)
	go func() {
		_, err := s.GetChunk(context.Background(), h)
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
	h := chunk.HashOf([]byte("expected"))
	sent := make(chan *miragev1.ChunkRequest, 1)
	s := New(func(f *miragev1.ServerFrame) error {
		sent <- f.GetChunkRequest()
		return nil
	})

	errCh := make(chan error, 1)
	go func() {
		_, err := s.GetChunk(context.Background(), h)
		errCh <- err
	}()

	req := <-sent
	// Server claims hash h but sends tampered bytes -> must fail verification.
	s.Dispatch(&miragev1.ChunkResponse{
		RequestId: req.GetRequestId(),
		Chunks:    []*miragev1.Chunk{{Hash: h[:], Data: []byte("tampered")}},
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

func TestGetChunkContextCancel(t *testing.T) {
	h := chunk.HashOf([]byte("never answered"))
	s := New(func(f *miragev1.ServerFrame) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := s.GetChunk(ctx, h)
		errCh <- err
	}()
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected context cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetChunk did not honor context cancellation")
	}
}
