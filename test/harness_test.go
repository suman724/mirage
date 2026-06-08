package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	clienttransport "github.com/suman724/mirage/client/transport"
	servertransport "github.com/suman724/mirage/server/transport"
)

// TestFuseHarness exercises the full M2 loop end to end: a mount-mode server and
// a real client over localhost gRPC. The "harness" (this test) reads files from
// the FUSE mount; each cold read makes the SERVER fault chunks over the channel
// from the client, and a warm re-read is served without new network traffic. It
// SKIPS when no FUSE module is available (macOS without macFUSE); it runs in the
// Linux/Docker validation environment (task 2.5-val).
func TestFuseHarness(t *testing.T) {
	srcDir := filepath.Join("..", "testdata", "workspace")

	// Own the mount dir explicitly so teardown ordering is deterministic:
	// cancel client -> gs.Stop() (waits for the handler to unmount) -> remove.
	mountDir, err := os.MkdirTemp("", "mirage-mnt-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(mountDir) // registered first => runs last (after gs.Stop)

	// --- server in MOUNT mode: accept only, fault reads over the channel ---
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mounted := make(chan servertransport.MountInfo, 1)
	gs := grpc.NewServer()
	servertransport.NewMounter(mountDir, func(mi servertransport.MountInfo) { mounted <- mi }, quietLogger()).Register(gs)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop() // blocks until the Connect handler returns (after unmount)

	// --- client: dial out, publish, serve chunk requests ---
	c, err := clienttransport.Dial(lis.Addr().String(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // disconnects the client, which triggers the server unmount
	serveErr := make(chan error, 1)
	go func() { serveErr <- c.Serve(ctx, srcDir) }()

	// Wait for the mount to come up. If the client session ends first, the
	// server failed to mount — almost certainly no FUSE in this environment.
	var info servertransport.MountInfo
	select {
	case info = <-mounted:
	case err := <-serveErr:
		t.Skipf("mount not established (FUSE unavailable in this environment?): %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the workspace to mount")
	}

	// Read every published file THROUGH the mount and compare to the source.
	// These reads drive ChunkRequests from the server back to the client.
	expected := publishableFiles(t, srcDir)
	var anyFile string
	for rel, want := range expected {
		anyFile = rel
		got, err := readWithRetry(filepath.Join(info.Mountpoint, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %q via mount: %v", rel, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("file %q via mount differs (%d vs %d bytes)", rel, len(got), len(want))
		}
	}

	// The server must have faulted data over the wire (it has no other source),
	// and never more than the total number of chunk references.
	faulted := info.Requests()
	totalRefs := uint64(totalChunkRefs(t, srcDir))
	if faulted == 0 {
		t.Fatal("no chunks were faulted over the channel")
	}
	if faulted > totalRefs {
		t.Errorf("faulted %d chunks over the wire, more than the %d total references", faulted, totalRefs)
	}
	t.Logf("faulted %d chunks over the wire for %d total references", faulted, totalRefs)

	// Warm read: reading a file again must not fault anything new — the cache
	// (and the kernel page cache) serve it. This is the M2 "second read hits
	// cache" property.
	if _, err := os.ReadFile(filepath.Join(info.Mountpoint, filepath.FromSlash(anyFile))); err != nil {
		t.Errorf("warm re-read %q: %v", anyFile, err)
	}
	if info.Requests() != faulted {
		t.Errorf("warm re-read faulted new chunks: %d -> %d", faulted, info.Requests())
	}
}

// readWithRetry reads a file, retrying briefly to absorb any mount-readiness lag.
func readWithRetry(path string) ([]byte, error) {
	var err error
	for i := 0; i < 20; i++ {
		var b []byte
		b, err = os.ReadFile(path)
		if err == nil {
			return b, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, err
}
