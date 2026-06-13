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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/folbricht/desync"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/fsutil"
	"github.com/suman724/mirage/internal/logging"
	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
	"github.com/suman724/mirage/server/channelstore"
	miragefuse "github.com/suman724/mirage/server/fuse"
	"github.com/suman724/mirage/server/seccomp"
	"github.com/suman724/mirage/server/shim"
)

// defaultSeccompWorkers is the notification-handler pool size when unset.
const defaultSeccompWorkers = 8

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

// ShimInfo is reported once a connection's workspace skeleton is built and
// the shim supervisor is accepting ENSURE requests (shim mode, design doc
// docs/design-shimmer.md).
type ShimInfo struct {
	Root       string        // workspace root holding the skeleton (symlink-resolved)
	SocketPath string        // supervisor unix socket (export as MIRAGE_SHIM_SOCK)
	StateDir   string        // journal + chunk cache location for this session
	Requests   func() uint64 // live count of chunks faulted over the wire so far
}

// SeccompInfo is reported once a connection's workspace skeleton is built and
// the seccomp supervisor is servicing the workload's open() traps (seccomp
// mode, design §3.3). This is the production Shimmer mode: mirage-server is the
// container entrypoint (PID 1), launches the workload under the C launcher
// (so the server is an ancestor — the ptrace_scope=1 requirement), and
// materializes files as the workload opens them.
type SeccompInfo struct {
	Root     string        // workspace root holding the skeleton (symlink-resolved)
	StateDir string        // journal + chunk cache location for this session
	Workload []string      // the command launched under interception
	Requests func() uint64 // live count of chunks faulted over the wire so far
}

// Server implements miragev1.MirageServer. Per connection it either
// reconstructs the published index into outDir (reconstruct mode) or FUSE-mounts
// it at mountDir so reads fault chunks lazily (mount mode).
type Server struct {
	miragev1.UnimplementedMirageServer
	outDir     string
	mountDir   string
	shimDir    string
	seccompDir string   // seccomp mode: workspace projection root
	stateDir   string   // shim/seccomp mode: journal + cache + socket; empty = per-connection temp
	launcher   string   // seccomp mode: path to the mirage-launcher binary
	workload   []string // seccomp mode: command run under interception
	workers    int      // seccomp mode: concurrent notification handlers (0 => default)
	sandboxID  string
	onResult   func(Result)
	onMounted  func(MountInfo)
	onShim     func(ShimInfo)
	onSeccomp  func(SeccompInfo)
	log        *slog.Logger
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

// NewShimmer returns a Server that projects each published workspace into
// shimDir as a skeleton of sparse placeholders and serves lazy per-file
// materialization over a unix socket (Shimmer: the FUSE-free mode for
// platforms like Fargate, zero kernel privileges needed). stateDir holds the
// state journal, the chunk cache, and the socket; if empty, a temp dir is
// used and the session is not restart-recoverable. onShim may be nil; if set
// it is invoked once the supervisor is accepting requests. logger may be nil.
func NewShimmer(shimDir, stateDir string, onShim func(ShimInfo), logger *slog.Logger) *Server {
	return &Server{
		shimDir:   shimDir,
		stateDir:  stateDir,
		sandboxID: "mirage-sandbox-0",
		onShim:    onShim,
		log:       logging.OrDefault(logger),
	}
}

// NewSeccomp returns a Server running the production Shimmer mode: it projects
// each published workspace into seccompDir as a sparse skeleton and runs as the
// seccomp supervisor, materializing files as the workload opens them (covering
// every binary — libc, Go, static — at the syscall layer; design §3.3).
//
// On each connection, after the skeleton is built, the server spawns
// `launcher workload...` as its own CHILD — so mirage-server is an ancestor of
// the workload, which ptrace_scope=1 requires for reading the trapped process's
// memory. Deploy mirage-server as the container entrypoint (PID 1) so every
// process in the task stays its descendant (see the §3.3 deployment note).
//
// stateDir holds the state journal, chunk cache, and the launcher hand-off
// socket; empty uses a per-connection temp dir (no restart recovery). workers
// is the notification-handler pool size (0 => a sensible default). onSeccomp
// may be nil. logger may be nil.
func NewSeccomp(seccompDir, stateDir, launcher string, workload []string, workers int, onSeccomp func(SeccompInfo), logger *slog.Logger) *Server {
	return &Server{
		seccompDir: seccompDir,
		stateDir:   stateDir,
		launcher:   launcher,
		workload:   workload,
		workers:    workers,
		sandboxID:  "mirage-sandbox-0",
		onSeccomp:  onSeccomp,
		log:        logging.OrDefault(logger),
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
	//
	// In shim mode the cache lives in the session state dir (alongside the
	// journal) so a configured --shim-state survives restarts; other modes —
	// and shim mode without a state dir — use a per-connection temp dir.
	cs := channelstore.New(ctx, send, log)
	stateDir, cacheDir, cleanup, err := s.sessionDirs()
	if err != nil {
		log.Error("failed to prepare session dirs", "err", err)
		return fmt.Errorf("transport: session dirs: %w", err)
	}
	defer cleanup()
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
		res := s.drive(ctx, manifest, storeChain, cs, stateDir)

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
	switch {
	case s.mountDir != "":
		return "mount"
	case s.shimDir != "":
		return "shim"
	case s.seccompDir != "":
		return "seccomp"
	default:
		return "reconstruct"
	}
}

// projecting reports whether this server projects a real skeleton on disk
// (shim or seccomp mode), which needs a state dir for the journal + cache.
func (s *Server) projecting() bool { return s.shimDir != "" || s.seccompDir != "" }

// sessionDirs resolves where this connection's state journal (shim mode) and
// chunk cache live, returning a cleanup for whatever is session-scoped. A
// configured shim state dir persists (restart recovery); everything else is
// a temp dir removed when the connection ends.
func (s *Server) sessionDirs() (stateDir, cacheDir string, cleanup func(), err error) {
	cleanup = func() {}
	switch {
	case s.projecting() && s.stateDir != "":
		stateDir = s.stateDir
		// 0700: the state dir holds the supervisor socket and is its trust
		// boundary (design §4).
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return "", "", nil, fmt.Errorf("create state dir %q: %w", stateDir, err)
		}
	case s.projecting():
		tmp, err := os.MkdirTemp("", "mirage-shim-")
		if err != nil {
			return "", "", nil, fmt.Errorf("create temp state dir: %w", err)
		}
		stateDir = tmp
		cleanup = func() { os.RemoveAll(tmp) }
	default:
		tmp, err := os.MkdirTemp("", "mirage-cache-")
		if err != nil {
			return "", "", nil, fmt.Errorf("create cache dir: %w", err)
		}
		cleanup = func() { os.RemoveAll(tmp) }
		return "", tmp, cleanup, nil
	}
	cacheDir = filepath.Join(stateDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("create cache dir %q: %w", cacheDir, err)
	}
	return stateDir, cacheDir, cleanup, nil
}

// drive runs the per-connection work to completion: FUSE-mount the workspace
// (mount mode), project it as a lazy skeleton (shim mode), or reconstruct it
// into outDir (reconstruct mode). It returns only after any cleanup
// (e.g. unmount, supervisor shutdown) is done.
func (s *Server) drive(ctx context.Context, m *chunk.Manifest, store desync.Store, cs *channelstore.Store, stateDir string) Result {
	switch {
	case s.mountDir != "":
		return s.serveMount(ctx, m, store, cs)
	case s.shimDir != "":
		return s.serveShim(ctx, m, store, cs, stateDir)
	case s.seccompDir != "":
		return s.serveSeccomp(ctx, m, store, cs, stateDir)
	default:
		return s.reconstruct(ctx, m, store)
	}
}

// serveShim builds the skeleton under shimDir and serves lazy per-file
// materialization over the supervisor socket until the client disconnects
// (Shimmer S1, docs/design-shimmer.md §3). Unlike mount mode there is nothing
// to unmount: the workspace is a real directory; whatever was materialized or
// locally written simply remains on disk.
func (s *Server) serveShim(ctx context.Context, m *chunk.Manifest, store desync.Store, cs *channelstore.Store, stateDir string) Result {
	res := Result{TotalRefs: uint64(m.TotalChunks())}

	if err := os.MkdirAll(s.shimDir, 0o755); err != nil {
		res.Err = fmt.Errorf("transport: create shim dir %q: %w", s.shimDir, err)
		return res
	}
	table, err := shim.OpenTable(filepath.Join(stateDir, "journal.jsonl"), s.log)
	if err != nil {
		res.Err = fmt.Errorf("transport: open shim state: %w", err)
		return res
	}
	defer func() {
		if err := table.Close(); err != nil {
			s.log.Warn("close shim state journal", "err", err)
		}
	}()

	buildTime := time.Now()
	skel, err := shim.BuildSkeleton(s.shimDir, m, table, buildTime, s.log)
	if err != nil {
		res.Err = fmt.Errorf("transport: build skeleton: %w", err)
		return res
	}

	sup, err := shim.NewSupervisor(shim.Config{
		Root:          s.shimDir,
		SocketPath:    filepath.Join(stateDir, "shim.sock"),
		Manifest:      m,
		Store:         store,
		Table:         table,
		BuildTime:     buildTime,
		ChunkRequests: cs.Requests,
		Logger:        s.log,
	})
	if err != nil {
		res.Err = fmt.Errorf("transport: start shim supervisor: %w", err)
		return res
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- sup.Serve() }()

	if s.onShim != nil {
		s.onShim(ShimInfo{
			Root:       sup.Root(),
			SocketPath: sup.SocketPath(),
			StateDir:   stateDir,
			Requests:   cs.Requests,
		})
	}

	// Serve ENSUREs until the client disconnects (stream context done) or the
	// supervisor itself dies.
	select {
	case <-ctx.Done():
		if err := sup.Close(); err != nil {
			s.log.Warn("close shim supervisor", "err", err)
		}
		<-serveErr
	case err := <-serveErr:
		if cerr := sup.Close(); cerr != nil {
			s.log.Warn("close shim supervisor", "err", cerr)
		}
		if err != nil {
			res.Err = fmt.Errorf("transport: shim supervisor: %w", err)
		}
	}

	res.Files = skel.Files
	res.Bytes = skel.Bytes
	return res
}

// serveSeccomp is the production Shimmer driver (design §3.3). It builds the
// skeleton, then launches the workload under the C launcher as a CHILD of this
// process and services its open() traps via seccomp user-notification —
// materializing files through the same shim.Materializer the socket front-end
// uses. Because the launcher is our child, mirage-server is an ancestor of the
// workload, satisfying the ptrace_scope=1 memory-read requirement (run
// mirage-server as PID 1 so daemonized descendants stay in the tree).
//
// Lifecycle: serve until the workload exits or the client disconnects, then
// stop the supervisor. Unlike mount mode there is nothing to unmount; whatever
// was materialized or written stays on disk.
func (s *Server) serveSeccomp(ctx context.Context, m *chunk.Manifest, store desync.Store, cs *channelstore.Store, stateDir string) Result {
	res := Result{TotalRefs: uint64(m.TotalChunks())}
	if len(s.workload) == 0 {
		res.Err = fmt.Errorf("transport: seccomp mode requires a workload command")
		return res
	}
	if err := os.MkdirAll(s.seccompDir, 0o755); err != nil {
		res.Err = fmt.Errorf("transport: create seccomp dir %q: %w", s.seccompDir, err)
		return res
	}
	table, err := shim.OpenTable(filepath.Join(stateDir, "journal.jsonl"), s.log)
	if err != nil {
		res.Err = fmt.Errorf("transport: open seccomp state: %w", err)
		return res
	}
	defer func() {
		if err := table.Close(); err != nil {
			s.log.Warn("close seccomp state journal", "err", err)
		}
	}()

	buildTime := time.Now()
	skel, err := shim.BuildSkeleton(s.seccompDir, m, table, buildTime, s.log)
	if err != nil {
		res.Err = fmt.Errorf("transport: build skeleton: %w", err)
		return res
	}
	res.Files = skel.Files
	res.Bytes = skel.Bytes

	mat, err := shim.NewMaterializer(s.seccompDir, m, store, table, buildTime, s.log)
	if err != nil {
		res.Err = fmt.Errorf("transport: build materializer: %w", err)
		return res
	}
	sup, err := seccomp.New(mat, s.log)
	if err != nil {
		// On non-Linux this is seccomp.ErrUnsupported (the mode is Linux-only).
		res.Err = fmt.Errorf("transport: start seccomp supervisor: %w", err)
		return res
	}

	// Unix socket on which the launcher hands us the seccomp listener fd.
	sockPath := filepath.Join(stateDir, "launcher.sock")
	_ = os.Remove(sockPath) // clear any stale socket
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		res.Err = fmt.Errorf("transport: listen launcher socket: %w", err)
		return res
	}
	defer ln.Close()

	// Spawn launcher+workload as OUR child (so we are its ancestor). The
	// launcher installs the seccomp filter, hands us the listener fd over
	// sockPath, waits for our ack, then execs the workload.
	child := exec.Command(s.launcher, s.workload...)
	child.Env = append(os.Environ(), "MIRAGE_SUPERVISOR_SOCK="+sockPath)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		res.Err = fmt.Errorf("transport: start launcher %q: %w", s.launcher, err)
		return res
	}

	listenerFd, conn, err := seccomp.RecvListenerFd(ln)
	if err != nil {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
		res.Err = fmt.Errorf("transport: receive listener fd: %w", err)
		return res
	}

	workers := s.workers
	if workers <= 0 {
		workers = defaultSeccompWorkers
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- sup.Serve(listenerFd, workers) }()

	// Ack the launcher so it execs the workload — only now that the supervisor
	// is servicing, so the workload's first open is answered, not dropped.
	if _, err := conn.Write([]byte("OK\n")); err != nil {
		s.log.Warn("ack launcher", "err", err)
	}
	conn.Close()

	if s.onSeccomp != nil {
		s.onSeccomp(SeccompInfo{
			Root:     mat.Root(),
			StateDir: stateDir,
			Workload: s.workload,
			Requests: cs.Requests,
		})
	}

	// Run until the workload exits or the client disconnects.
	cmdErr := make(chan error, 1)
	go func() { cmdErr <- child.Wait() }()
	select {
	case err := <-cmdErr:
		if err != nil {
			s.log.Info("workload exited", "err", err)
		} else {
			s.log.Info("workload exited cleanly")
		}
	case <-ctx.Done():
		s.log.Info("client disconnected; terminating workload")
		_ = child.Process.Kill()
		<-cmdErr
	}

	sup.Stop()
	<-serveErr
	_ = syscall.Close(listenerFd)
	return res
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
		dst, err := fsutil.SafeJoin(s.outDir, f.Path)
		if err != nil {
			res.Err = fmt.Errorf("transport: %w", err)
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
