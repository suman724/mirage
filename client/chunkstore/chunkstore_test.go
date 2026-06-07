package chunkstore

import (
	"testing"

	"github.com/suman724/mirage/internal/chunk"
)

func TestPutGetHas(t *testing.T) {
	s := New()
	data := []byte("chunk bytes")
	h := chunk.HashOf(data)

	if s.Has(h) {
		t.Fatalf("empty store should not have hash")
	}
	if _, found := s.Get(h); found {
		t.Fatalf("empty store should not find hash")
	}

	s.Put(h, data)
	if !s.Has(h) {
		t.Fatalf("store should have hash after Put")
	}
	got, found := s.Get(h)
	if !found {
		t.Fatalf("store should find hash after Put")
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", got, data)
	}
	if s.Len() != 1 {
		t.Fatalf("Len=%d, want 1", s.Len())
	}
}

func TestRejectUnknownHash(t *testing.T) {
	s := New()
	s.Put(chunk.HashOf([]byte("published")), []byte("published"))

	// A hash that was never published (e.g. a secret file's chunk) is rejected.
	secret := chunk.HashOf([]byte("SUPER_SECRET=do-not-publish"))
	if _, found := s.Get(secret); found {
		t.Fatalf("store must NOT serve an unpublished hash")
	}
}
