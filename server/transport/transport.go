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

	"github.com/folbricht/desync"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/logging"
	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
	"github.com/suman724/mirage/server/channelstore"
	miragefuse "github.com/suman724/mirage/server/fuse"
)

// Result summarizes one completed reconstruction. It is delivered to the
// OnResult callback (used by tests and the CLI) once a connection's index has
// been fully reconstructed.
type Result struct {
	OutDir        string
	Files         int
	Bytes         uint64
	TotalRefs     uint64 // total chunk references across all files (with duplicates)
	ChunkRequests uint64 // ChunkRequests originated over the channel (distinct chunks faulted)
	CacheHits     uint64 // chunk reads served from the local cache (duplicates)
	Err           error
}

// MountInfo is reported once a connection's workspace has been FUSE-mounted.
type MountInfo struct {
	Mountpoint string        // directory the workspace is mounted at
	Requests   func() uint64 // live count of chunks faulted over the wire so far
}

// Server implements miragev1.MirageServer. Per connection it either
// reconstructs the published index into outDir (reconstruct mode) or FUSE-mounts
// it at mountDir so reads fault chunks lazily (mount mode).
type Server struct {
	miragev1.UnimplementedMirageServer
	outDir    string
	mountDir  string
	sandboxID string
	onResult  func(Result)
	onMounted func(MountInfo)
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

// NewMounter returns a Server that FUSE-mounts each published workspace at
// mountDir, so a real POSIX read faults chunks over the channel (the M2 goal).
// onMounted may be nil; if set it is invoked once the mount is live. The mount
// stays up until the client disconnects. logger may be nil.
func NewMounter(mountDir string, onMounted func(MountInfo), logger *slog.Logger) *Server {
	return &Server{
		mountDir:  mountDir,
		sandboxID: "mirage-sandbox-0",
		onMounted: onMounted,
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

	// Store chain (design §4.1, reusing desync): reconstruction reads through a
	// local disk Cache, whose misses fall through a DedupQueue (single-flight)
	// to the channelstore, which faults the chunk down the open stream.
	//   cache(local) -> dedup -> channelstore -> ChunkRequest over the wire
	cs := channelstore.New(ctx, send, log)
	cacheDir, err := os.MkdirTemp("", "mirage-cache-")
	if err != nil {
		log.Error("failed to create chunk cache dir", "err", err)
		return fmt.Errorf("transport: create cache dir: %w", err)
	}
	defer os.RemoveAll(cacheDir)
	localCache, err := desync.NewLocalStore(cacheDir, desync.StoreOptions{})
	if err != nil {
		log.Error("failed to open local chunk cache", "dir", cacheDir, "err", err)
		return fmt.Errorf("transport: open local cache: %w", err)
	}
	storeChain := desync.NewCache(desync.NewDedupQueue(cs), localCache)

	// The recv loop runs concurrently for the whole connection: it answers the
	// Hello, forwards the published manifest to the driver, and (crucially)
	// keeps dispatching ChunkResponses to the channelstore so faults are
	// answered while a reconstruction or mount is in progress.
	indexCh := make(chan *chunk.Manifest, 1)
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
					recvErr <- fmt.Errorf("parse index: %w", err)
					return
				}
				log.Info("index published",
					"mode", s.mode(),
					"files", len(manifest.Files),
					"total_chunks", ip.GetTotalChunks(),
					"unique_chunks", len(manifest.UniqueHashes()),
					"total_bytes", ip.GetTotalBytes())
				select {
				case indexCh <- manifest:
				default: // ignore a second IndexPublish on the same connection
				}
			case *miragev1.ClientFrame_ChunkResponse:
				cs.Dispatch(p.ChunkResponse)
			default:
				log.Debug("ignoring out-of-scope client frame")
			}
		}
	}()

	select {
	case manifest := <-indexCh:
		// Drive synchronously: the driver owns the full lifecycle (including, in
		// mount mode, unmounting on disconnect) so the handler does not return —
		// and the connection's resources are not reclaimed — until cleanup is
		// done. The recv goroutine keeps dispatching ChunkResponses meanwhile.
		res := s.drive(ctx, manifest, storeChain, cs)

		// Channel fetches (= distinct chunks faulted over the wire) vs cache
		// hits (refs served from the local cache). desync.Cache exposes no
		// counters, so derive hits from total refs minus channel fetches.
		res.ChunkRequests = cs.Requests()
		if res.TotalRefs >= res.ChunkRequests {
			res.CacheHits = res.TotalRefs - res.ChunkRequests
		}
		if res.Err != nil {
			log.Error("session failed",
				"err", res.Err, "files", res.Files,
				"chunk_requests", res.ChunkRequests, "cache_hits", res.CacheHits)
		} else {
			log.Info("session complete",
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

// mode reports the per-connection driver mode for logging.
func (s *Server) mode() string {
	if s.mountDir != "" {
		return "mount"
	}
	return "reconstruct"
}

// drive runs the per-connection work to completion: FUSE-mount the workspace
// (mount mode) or reconstruct it into outDir (reconstruct mode). It returns
// only after any cleanup (e.g. unmount) is done.
func (s *Server) drive(ctx context.Context, m *chunk.Manifest, store desync.Store, cs *channelstore.Store) Result {
	if s.mountDir != "" {
		return s.serveMount(ctx, m, store, cs)
	}
	return s.reconstruct(ctx, m, store)
}

// serveMount FUSE-mounts the manifest at mountDir, backed by the store chain,
// and serves lazy reads (each cold read faults chunks over the channel) until
// the connection's context is cancelled, then unmounts. This is the M2 path: a
// real POSIX read on the sandbox faults chunks via ChunkRequest.
func (s *Server) serveMount(ctx context.Context, m *chunk.Manifest, store desync.Store, cs *channelstore.Store) Result {
	res := Result{TotalRefs: uint64(m.TotalChunks())}
	mount, err := miragefuse.New(s.mountDir, m, store, s.log)
	if err != nil {
		res.Err = fmt.Errorf("transport: mount workspace: %w", err)
		return res
	}
	if s.onMounted != nil {
		s.onMounted(MountInfo{Mountpoint: mount.Mountpoint(), Requests: cs.Requests})
	}

	// Stay mounted, faulting reads over the channel, until the client
	// disconnects (stream context done).
	<-ctx.Done()

	if err := mount.Unmount(); err != nil {
		s.log.Warn("unmount failed", "err", err)
	}
	res.Files = len(m.Files)
	return res
}

// reconstruct materializes every file in the manifest into outDir, faulting
// each chunk's bytes through the provided desync store chain (cache -> dedup ->
// channelstore). Transport-level metrics (channel requests, cache hits) are
// filled by the caller from the underlying channelstore.
func (s *Server) reconstruct(ctx context.Context, m *chunk.Manifest, store desync.Store) Result {
	start := time.Now()
	res := Result{OutDir: s.outDir, TotalRefs: uint64(m.TotalChunks())}
	if err := os.MkdirAll(s.outDir, 0o755); err != nil {
		res.Err = fmt.Errorf("transport: create out dir %q: %w", s.outDir, err)
		return res
	}
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			res.Err = fmt.Errorf("transport: reconstruction cancelled: %w", err)
			break
		}
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
			ck, err := store.GetChunk(desync.ChunkID(ref.Hash))
			if err != nil {
				res.Err = fmt.Errorf("transport: fault chunk for %q: %w", f.Path, err)
				break
			}
			data, err := ck.Data()
			if err != nil {
				res.Err = fmt.Errorf("transport: decode chunk for %q: %w", f.Path, err)
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
