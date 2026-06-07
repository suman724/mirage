package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suman724/mirage/internal/chunk"
)

// fakeBacking is a controllable chunk.Store standing in for the channelstore.
type fakeBacking struct {
	mu    sync.Mutex
	data  map[chunk.Hash][]byte
	calls atomic.Uint64
	err   error
	gate  chan struct{} // if non-nil, GetChunk blocks until closed
}

func (f *fakeBacking) GetChunk(ctx context.Context, h chunk.Hash) ([]byte, error) {
	f.calls.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.data[h]
	if !ok {
		return nil, errors.New("not found")
	}
	return d, nil
}

func TestCacheHitAfterMiss(t *testing.T) {
	data := []byte("chunk contents")
	h := chunk.HashOf(data)
	b := &fakeBacking{data: map[chunk.Hash][]byte{h: data}}
	c := New(b, nil)

	// First read: miss -> backing fetch.
	got, err := c.GetChunk(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", got, data)
	}
	// Second read: hit -> no new backing call.
	if _, err := c.GetChunk(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if b.calls.Load() != 1 {
		t.Fatalf("backing called %d times, want 1 (second read should hit cache)", b.calls.Load())
	}
	hits, misses := c.Stats()
	if hits != 1 || misses != 1 {
		t.Fatalf("stats hits=%d misses=%d, want 1/1", hits, misses)
	}
	if c.Len() != 1 {
		t.Fatalf("cache Len=%d, want 1", c.Len())
	}
}

func TestCacheSingleFlightCoalescesConcurrentMisses(t *testing.T) {
	data := []byte("shared chunk")
	h := chunk.HashOf(data)
	gate := make(chan struct{})
	b := &fakeBacking{data: map[chunk.Hash][]byte{h: data}, gate: gate}
	c := New(b, nil)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.GetChunk(context.Background(), h)
		}(i)
	}
	// Let all goroutines reach the single-flight before releasing the backing.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := b.calls.Load(); got != 1 {
		t.Fatalf("backing called %d times, want 1 (concurrent misses must coalesce)", got)
	}
}

func TestCacheDoesNotCacheErrors(t *testing.T) {
	h := chunk.HashOf([]byte("missing"))
	b := &fakeBacking{data: map[chunk.Hash][]byte{}, err: errors.New("boom")}
	c := New(b, nil)

	if _, err := c.GetChunk(context.Background(), h); err == nil {
		t.Fatal("expected error from backing")
	}
	if _, err := c.GetChunk(context.Background(), h); err == nil {
		t.Fatal("expected error on retry too")
	}
	if b.calls.Load() != 2 {
		t.Fatalf("backing called %d times, want 2 (errors must not be cached)", b.calls.Load())
	}
	if _, misses := c.Stats(); misses != 0 {
		t.Fatalf("failed fetches must not count as misses, got %d", misses)
	}
}
