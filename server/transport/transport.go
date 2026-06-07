// Package transport is the SERVER side of the Mirage connection. It ONLY ever
// accepts the client-initiated gRPC stream — it never dials (design §3, §10).
// Once the client opens the stream and publishes an index, the server drives
// the protocol: it originates ChunkRequests over that same stream (via
// channelstore) and reconstructs the published files into an output directory.
//
// The server has no access to the client's source directory. The only way it
// can obtain file bytes is by requesting chunk hashes that appear in the
// published manifest — which is exactly the property this spike proves.
package transport

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/grpc"

	"github.com/suman724/mirage/internal/chunk"
	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
	"github.com/suman724/mirage/server/channelstore"
)

// Result summarizes one completed reconstruction. It is delivered to the
// OnResult callback (used by tests and the CLI) once a connection's index has
// been fully reconstructed.
type Result struct {
	OutDir        string
	Files         int
	Bytes         uint64
	ChunkRequests uint64 // ChunkRequests originated over the channel
	Err           error
}

// Server implements miragev1.MirageServer. It reconstructs each published
// index into OutDir.
type Server struct {
	miragev1.UnimplementedMirageServer
	outDir    string
	sandboxID string
	onResult  func(Result)
}

// New returns a Server that reconstructs published trees into outDir. onResult
// may be nil; if set it is invoked once per completed connection.
func New(outDir string, onResult func(Result)) *Server {
	return &Server{outDir: outDir, sandboxID: "mirage-sandbox-0", onResult: onResult}
}

// Register attaches the service to a gRPC server.
func (s *Server) Register(gs *grpc.Server) {
	miragev1.RegisterMirageServer(gs, s)
}

// Connect handles one client-initiated bidi stream for its whole lifetime.
func (s *Server) Connect(stream miragev1.Mirage_ConnectServer) error {
	ctx := stream.Context()

	// Serialize all sends; both the recv loop (HelloAck) and the channelstore
	// (ChunkRequest) write to the stream.
	var sendMu sync.Mutex
	send := func(f *miragev1.ServerFrame) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(f)
	}

	cs := channelstore.New(send)
	reconDone := make(chan Result, 1)
	recvErr := make(chan error, 1)

	go func() {
		for {
			frame, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			switch p := frame.Payload.(type) {
			case *miragev1.ClientFrame_Hello:
				if err := send(&miragev1.ServerFrame{
					Payload: &miragev1.ServerFrame_HelloAck{
						HelloAck: &miragev1.HelloAck{SandboxId: s.sandboxID},
					},
				}); err != nil {
					recvErr <- err
					return
				}
			case *miragev1.ClientFrame_IndexPublish:
				manifest, err := chunk.Unmarshal(p.IndexPublish.GetCaidx())
				if err != nil {
					reconDone <- Result{OutDir: s.outDir, Err: fmt.Errorf("parse index: %w", err)}
					return
				}
				go func() {
					reconDone <- s.reconstruct(ctx, manifest, cs)
				}()
			case *miragev1.ClientFrame_ChunkResponse:
				cs.Dispatch(p.ChunkResponse)
			default:
				// Heartbeats and out-of-scope frames are ignored this round.
			}
		}
	}()

	select {
	case res := <-reconDone:
		if s.onResult != nil {
			s.onResult(res)
		}
		return res.Err // returning closes the stream; client sees io.EOF
	case err := <-recvErr:
		if err == io.EOF {
			return nil
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reconstruct materializes every file in the manifest into outDir, fetching
// each chunk's bytes over the channel via the channelstore.
func (s *Server) reconstruct(ctx context.Context, m *chunk.Manifest, cs *channelstore.Store) Result {
	res := Result{OutDir: s.outDir}
	if err := os.MkdirAll(s.outDir, 0o755); err != nil {
		res.Err = err
		res.ChunkRequests = cs.Requests()
		return res
	}
	for _, f := range m.Files {
		dst, err := safeJoin(s.outDir, f.Path)
		if err != nil {
			res.Err = err
			break
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			res.Err = err
			break
		}
		var buf []byte
		for _, ref := range f.Chunks {
			data, err := cs.GetChunk(ctx, ref.Hash)
			if err != nil {
				res.Err = err
				break
			}
			buf = append(buf, data...)
		}
		if res.Err != nil {
			break
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(dst, buf, mode); err != nil {
			res.Err = err
			break
		}
		res.Files++
		res.Bytes += uint64(len(buf))
	}
	res.ChunkRequests = cs.Requests()
	return res
}

// safeJoin joins a relative path onto root, rejecting traversal outside root.
func safeJoin(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	rootAbs := filepath.Clean(root)
	if clean != rootAbs && !strings.HasPrefix(clean, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("transport: path %q escapes output root", rel)
	}
	return clean, nil
}
