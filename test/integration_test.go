// Package integration exercises the full client<->server data path over a real
// localhost gRPC connection, proving the milestone DoD:
//
//  1. the client dials the server and opens one bidi stream (Mirage.Connect),
//  2. the client publishes an index for a test directory,
//  3. the SERVER originates ChunkRequests down that same stream,
//  4. the client serves chunk bytes by hash (and rejects unpublished hashes),
//  5. the server reconstructs the files and we verify them byte-for-byte
//     against the source — with the server having read ONLY chunks off the
//     stream, never the source directory.
package integration

import (
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/suman724/mirage/client/index"
	clienttransport "github.com/suman724/mirage/client/transport"
	servertransport "github.com/suman724/mirage/server/transport"
)

func TestEndToEndReconstruction(t *testing.T) {
	srcDir := filepath.Join("..", "testdata", "workspace")
	outDir := t.TempDir()

	// --- server: ACCEPT only, never dial ---
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan servertransport.Result, 1)
	gs := grpc.NewServer()
	servertransport.New(outDir, func(r servertransport.Result) { resultCh <- r }).Register(gs)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	// --- client: DIAL out, publish, serve chunks ---
	c, err := clienttransport.Dial(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	serveErr := make(chan error, 1)
	go func() { serveErr <- c.Serve(context.Background(), srcDir) }()

	var result servertransport.Result
	select {
	case result = <-resultCh:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for server reconstruction")
	}
	if result.Err != nil {
		t.Fatalf("reconstruction error: %v", result.Err)
	}

	// The server must have obtained data ONLY via the channel.
	if result.ChunkRequests == 0 {
		t.Fatal("server reconstructed without originating any ChunkRequest")
	}

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("client serve error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not finish after server closed the stream")
	}

	assertTreesEqual(t, srcDir, outDir)
}

// assertTreesEqual checks that outDir contains exactly the publishable files of
// srcDir (secrets/.git excluded) with byte-identical contents.
func assertTreesEqual(t *testing.T, srcDir, outDir string) {
	t.Helper()

	expected := map[string][]byte{}
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || index.IsSecret(d.Name()) {
			return nil
		}
		rel, _ := filepath.Rel(srcDir, path)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		expected[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) == 0 {
		t.Fatal("no expected files found; testdata missing?")
	}

	// Every expected file must be reconstructed byte-for-byte.
	for rel, want := range expected {
		got, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("missing reconstructed file %q: %v", rel, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("file %q differs (%d vs %d bytes)", rel, len(got), len(want))
		}
	}

	// Secrets must NOT have been reconstructed (never published).
	for _, secret := range []string{".env", "id_rsa"} {
		if _, err := os.Stat(filepath.Join(outDir, secret)); err == nil {
			t.Errorf("secret %q must never be reconstructed on the server", secret)
		}
	}

	// The server must not have invented extra files.
	got := map[string]bool{}
	_ = filepath.WalkDir(outDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(outDir, path)
		got[filepath.ToSlash(rel)] = true
		return nil
	})
	for rel := range got {
		if _, ok := expected[rel]; !ok {
			t.Errorf("unexpected reconstructed file %q", rel)
		}
	}
}
