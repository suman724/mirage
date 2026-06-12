package integration

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	clienttransport "github.com/suman724/mirage/client/transport"
	servertransport "github.com/suman724/mirage/server/transport"
)

// TestShimHarness exercises the Shimmer S1 loop end to end over real
// localhost gRPC, playing the role the C LD_PRELOAD shim will play in S2:
//
//  1. the client dials out and publishes testdata/workspace,
//  2. the server (shim mode) builds a sparse skeleton — metadata is real,
//     zero chunks fetched,
//  3. this test sends ENSURE over the supervisor socket before reading a
//     file, exactly like the shim's intercepted open(),
//  4. the file materializes byte-identical, faulting only its own chunks
//     over the channel; everything untouched stays sparse,
//  5. MATERIALIZE_ALL degrades to --out semantics: the full tree verifies.
//
// Unlike the FUSE harness this needs no kernel module and runs everywhere —
// that is Shimmer's point (design G4).
func TestShimHarness(t *testing.T) {
	srcDir := filepath.Join("..", "testdata", "workspace")

	// Short paths: the supervisor socket lives under the state dir, and unix
	// socket paths are limited to ~104 bytes on darwin.
	shimRoot, err := os.MkdirTemp("", "mirage-ws-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(shimRoot)

	// --- server in SHIM mode: accept only, lazy ENSURE over the socket ---
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan servertransport.ShimInfo, 1)
	gs := grpc.NewServer()
	servertransport.NewShimmer(shimRoot, "",
		func(si servertransport.ShimInfo) { ready <- si }, quietLogger()).Register(gs)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	// --- client: dial out, publish, serve chunk requests ---
	c, err := clienttransport.Dial(lis.Addr().String(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- c.Serve(ctx, srcDir) }()

	var info servertransport.ShimInfo
	select {
	case info = <-ready:
	case err := <-serveErr:
		t.Fatalf("client session ended before the shim came up: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the shim supervisor")
	}

	expected := publishableFiles(t, srcDir)

	// 1. The skeleton is metadata-complete and fully lazy: every published
	// file exists with its true size, no blocks allocated, and NOTHING has
	// been faulted over the channel yet.
	for rel, want := range expected {
		path := filepath.Join(info.Root, filepath.FromSlash(rel))
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("skeleton missing %q: %v", rel, err)
		}
		if fi.Size() != int64(len(want)) {
			t.Errorf("%q apparent size = %d, want %d", rel, fi.Size(), len(want))
		}
		if blocks := allocatedBlocks(t, path); blocks != 0 {
			t.Errorf("%q allocated %d blocks before any ENSURE", rel, blocks)
		}
	}
	if n := info.Requests(); n != 0 {
		t.Fatalf("skeleton build faulted %d chunks; must be metadata-only", n)
	}

	// 2. ENSURE one file (what the shim's open() interception does), then
	// read it directly off the real filesystem.
	var oneFile string
	for rel := range expected {
		if oneFile == "" || rel < oneFile {
			oneFile = rel // deterministic pick
		}
	}
	absOne := filepath.Join(info.Root, filepath.FromSlash(oneFile))
	if resp := shimRequest(t, info.SocketPath, "ENSURE "+absOne); resp != "OK" {
		t.Fatalf("ENSURE %s = %q", oneFile, resp)
	}
	got, err := os.ReadFile(absOne)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(expected[oneFile]) {
		t.Fatalf("%q materialized %d bytes, differs from source (%d bytes)", oneFile, len(got), len(expected[oneFile]))
	}

	// Laziness: only that file's chunks crossed the wire; every other file
	// is still sparse.
	afterOne := info.Requests()
	if afterOne == 0 {
		t.Fatal("ENSURE faulted no chunks; the server has no other byte source")
	}
	if total := uint64(totalChunkRefs(t, srcDir)); afterOne >= total {
		t.Errorf("single-file ENSURE faulted %d chunks of %d total refs; not lazy", afterOne, total)
	}
	for rel := range expected {
		if rel == oneFile {
			continue
		}
		path := filepath.Join(info.Root, filepath.FromSlash(rel))
		if blocks := allocatedBlocks(t, path); blocks != 0 {
			t.Errorf("untouched %q allocated %d blocks after ensuring only %q", rel, blocks, oneFile)
		}
	}

	// 3. STATS reflects exactly one materialized file.
	stats := shimRequest(t, info.SocketPath, "STATS")
	if !strings.HasPrefix(stats, "OK ") || !strings.Contains(stats, "materialized=1") {
		t.Errorf("STATS = %q, want materialized=1", stats)
	}

	// 4. MATERIALIZE_ALL: the workspace degrades to --out semantics and the
	// whole tree must verify byte-for-byte (secrets still absent — they were
	// never published, the iron rule the shim path must not weaken).
	if resp := shimRequest(t, info.SocketPath, "MATERIALIZE_ALL"); !strings.HasPrefix(resp, "OK ") {
		t.Fatalf("MATERIALIZE_ALL = %q", resp)
	}
	assertTreesEqual(t, srcDir, info.Root)

	// The cache + dedup chain still elides duplicate chunks: total channel
	// fetches stay at the unique-chunk count.
	if unique := uint64(uniqueChunkCount(t, srcDir)); info.Requests() != unique {
		t.Errorf("channel fetches = %d, want %d (one per unique chunk)", info.Requests(), unique)
	}

	// 5. Disconnect. The workspace is a real directory — materialized
	// content survives the session (nothing to unmount).
	cancel()
	select {
	case <-serveErr:
	case <-time.After(10 * time.Second):
		t.Fatal("client did not shut down")
	}
	gs.Stop()
	if got, err := os.ReadFile(absOne); err != nil || string(got) != string(expected[oneFile]) {
		t.Errorf("materialized file did not survive disconnect: %v", err)
	}
}

// shimRequest performs one supervisor round trip — the role of the C shim.
func shimRequest(t *testing.T, sock, line string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		t.Fatalf("dial supervisor %s: %v", sock, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
		t.Fatalf("send %q: %v", line, err)
	}
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read reply to %q: %v", line, err)
	}
	return strings.TrimRight(resp, "\n")
}

// allocatedBlocks returns the file's allocated block count (0 = sparse).
func allocatedBlocks(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no syscall.Stat_t on this platform")
	}
	return st.Blocks
}
