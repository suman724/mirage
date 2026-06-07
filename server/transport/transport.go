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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/logging"
	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
	"github.com/suman724/mirage/server/cache"
	"github.com/suman724/mirage/server/channelstore"
)

// Result summarizes one completed reconstruction. It is delivered to the
// OnResult callback (used by tests and the CLI) once a connection's index has
// been fully reconstructed.
type Result struct {
	OutDir        string
	Files         int
	Bytes         uint64
	ChunkRequests uint64 // ChunkRequests originated over the channel (cache misses)
	CacheHits     uint64 // chunk reads served from the local cache
	Err           error
}

// Server implements miragev1.MirageServer. It reconstructs each published
// index into OutDir.
type Server struct {
	miragev1.UnimplementedMirageServer
	outDir    string
	sandboxID string
	onResult  func(Result)
	log       *slog.Logger
}

// New returns a Server that reconstructs published trees into outDir. onResult
// may be nil; if set it is invoked once per completed connection. logger may be
// nil (defaults to slog.Default()).
func New(outDir string, onResult func(Result), logger *slog.Logger) *Server {
	return &Server{
		outDir:    outDir,
		sandboxID: "mirage-sandbox-0",
		onResult:  onResult,
		log:       logging.OrDefault(logger),
	}
}

// Register attaches the service to a gRPC server.
func (s *Server) Register(gs *grpc.Server) {
	miragev1.RegisterMirageServer(gs, s)
}

// Connect handles one client-initiated bidi stream for its whole lifetime.
func (s *Server) Connect(stream miragev1.Mirage_ConnectServer) error {
	ctx := stream.Context()
	log := s.log
	if p, ok := peerAddr(ctx); ok {
		log = log.With("peer", p)
	}
	log.Info("client connected; accepting stream")

	// Serialize all sends; both the recv loop (HelloAck) and the channelstore
	// (ChunkRequest) write to the stream.
	var sendMu sync.Mutex
	send := func(f *miragev1.ServerFrame) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(f)
	}

	// Store chain: reconstruction reads through the local cache, which faults
	// misses through the channelstore down the open stream (design §4.1).
	cs := channelstore.New(send, log)
	cacheStore := cache.New(cs, log)
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
				h := p.Hello
				log.Info("received hello",
					"client_version", h.GetClientVersion(),
					"os", h.GetOs(),
					"workspace", h.GetWorkspace().GetRootName())
				if err := send(&miragev1.ServerFrame{
					Payload: &miragev1.ServerFrame_HelloAck{
						HelloAck: &miragev1.HelloAck{SandboxId: s.sandboxID},
					},
				}); err != nil {
					recvErr <- fmt.Errorf("send HelloAck: %w", err)
					return
				}
			case *miragev1.ClientFrame_IndexPublish:
				ip := p.IndexPublish
				manifest, err := chunk.Unmarshal(ip.GetCaidx())
				if err != nil {
					log.Error("failed to parse published index", "err", err)
					reconDone <- Result{OutDir: s.outDir, Err: fmt.Errorf("parse index: %w", err)}
					return
				}
				log.Info("index published; starting reconstruction",
					"files", len(manifest.Files),
					"total_chunks", ip.GetTotalChunks(),
					"unique_chunks", len(manifest.UniqueHashes()),
					"total_bytes", ip.GetTotalBytes(),
					"out_dir", s.outDir)
				go func() {
					reconDone <- s.reconstruct(ctx, manifest, cacheStore)
				}()
			case *miragev1.ClientFrame_ChunkResponse:
				cs.Dispatch(p.ChunkResponse)
			default:
				log.Debug("ignoring out-of-scope client frame")
			}
		}
	}()

	select {
	case res := <-reconDone:
		// Fill transport-level metrics: channel fetches (cache misses) vs hits.
		res.ChunkRequests = cs.Requests()
		res.CacheHits, _ = cacheStore.Stats()
		if res.Err != nil {
			log.Error("reconstruction failed",
				"err", res.Err, "files", res.Files,
				"chunk_requests", res.ChunkRequests, "cache_hits", res.CacheHits)
		} else {
			log.Info("reconstruction complete",
				"files", res.Files, "bytes", res.Bytes,
				"chunk_requests", res.ChunkRequests, "cache_hits", res.CacheHits)
		}
		if s.onResult != nil {
			s.onResult(res)
		}
		return res.Err // returning closes the stream; client sees io.EOF
	case err := <-recvErr:
		if err == io.EOF {
			log.Info("client closed the stream")
			return nil
		}
		log.Error("stream receive error", "err", err)
		return err
	case <-ctx.Done():
		log.Warn("connection context done", "err", ctx.Err())
		return ctx.Err()
	}
}

// reconstruct materializes every file in the manifest into outDir, faulting
// each chunk's bytes through the provided store chain (cache -> channelstore).
// Transport-level metrics (channel requests, cache hits) are filled by the
// caller from the underlying stores.
func (s *Server) reconstruct(ctx context.Context, m *chunk.Manifest, store chunk.Store) Result {
	start := time.Now()
	res := Result{OutDir: s.outDir}
	if err := os.MkdirAll(s.outDir, 0o755); err != nil {
		res.Err = fmt.Errorf("transport: create out dir %q: %w", s.outDir, err)
		return res
	}
	for _, f := range m.Files {
		dst, err := safeJoin(s.outDir, f.Path)
		if err != nil {
			res.Err = err
			break
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			res.Err = fmt.Errorf("transport: create dir for %q: %w", f.Path, err)
			break
		}
		buf := make([]byte, 0, fileSize(f))
		for _, ref := range f.Chunks {
			data, err := store.GetChunk(ctx, ref.Hash)
			if err != nil {
				res.Err = fmt.Errorf("transport: fault chunk for %q: %w", f.Path, err)
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
			res.Err = fmt.Errorf("transport: write %q: %w", f.Path, err)
			break
		}
		s.log.Debug("reconstructed file", "path", f.Path, "bytes", len(buf), "chunks", len(f.Chunks))
		res.Files++
		res.Bytes += uint64(len(buf))
	}
	s.log.Debug("reconstruction pass finished",
		"files", res.Files, "bytes", res.Bytes, "elapsed", time.Since(start))
	return res
}

// fileSize sums a file entry's chunk sizes for buffer pre-allocation.
func fileSize(f chunk.FileEntry) int {
	var n int
	for _, c := range f.Chunks {
		n += int(c.Size)
	}
	return n
}

// peerAddr extracts the client's network address from the stream context, if
// available, for logging.
func peerAddr(ctx context.Context) (string, bool) {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String(), true
	}
	return "", false
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
