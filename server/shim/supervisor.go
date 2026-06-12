package shim

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/folbricht/desync"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/logging"
)

// Protocol (design §4): newline-delimited text over a SOCK_STREAM unix
// socket, ONE request per connection, deliberately primitive so the C shim
// stays trivial. The socket's parent directory is the trust boundary (0700).
//
//	ENSURE <abs-path>   -> OK | ERR <msg>    materialize before a read
//	DIRTY <abs-path>    -> OK | ERR <msg>    content diverged (open-for-write)
//	MATERIALIZE_ALL     -> OK files=N | ERR <msg>
//	STATS               -> OK ensures=N dirty=N placeholder=N materialized=N
//	                          local=N torn=N errors=N chunk_requests=N
//
// NOTE (S3′ pivot, docs/design-shimmer.md §3.3): this LD_PRELOAD-shim socket
// front-end is superseded on Fargate by the seccomp notification loop, which
// reuses the same Materializer. It is retained as the fallback for
// environments where seccomp filter installs are forbidden.
const (
	verbEnsure         = "ENSURE"
	verbDirty          = "DIRTY"
	verbMaterializeAll = "MATERIALIZE_ALL"
	verbStats          = "STATS"

	// connTimeout bounds one request end to end (read, possibly a full-file
	// materialization over the channel, reply). Matches the design's 30s.
	connTimeout = 30 * time.Second

	// maxLine bounds a request line; PATH_MAX is 4096, so 64 KiB is generous.
	maxLine = 64 * 1024
)

// Config assembles a Supervisor. All fields except ChunkRequests and Logger
// are required.
type Config struct {
	// Root is the workspace directory holding the skeleton.
	Root string
	// SocketPath is where the supervisor listens. Its parent directory is
	// forced to 0700 (it is the trust boundary).
	SocketPath string
	// Manifest is the session-frozen published index.
	Manifest *chunk.Manifest
	// Store is the chunk source (the Cache(DedupQueue(channelstore), Local)
	// chain in production; an in-memory store in tests).
	Store desync.Store
	// Table is the state table, already replayed and skeleton-tracked.
	Table *Table
	// BuildTime is the skeleton's mtime stamp, re-applied after each fill so
	// materialization is mtime-invisible.
	BuildTime time.Time
	// ChunkRequests optionally reports chunks faulted over the wire so far
	// (channelstore.Requests) for STATS. May be nil.
	ChunkRequests func() uint64
	// Logger may be nil (defaults to slog.Default()).
	Logger *slog.Logger
}

// Supervisor is the unix-socket front-end over a Materializer: it serves
// ENSURE/DIRTY/MATERIALIZE_ALL/STATS requests from the C shim. It is safe for
// concurrent use.
type Supervisor struct {
	mat           *Materializer
	chunkRequests func() uint64
	log           *slog.Logger

	ln net.Listener
	wg sync.WaitGroup

	closed  atomic.Bool
	ensures atomic.Uint64
	dirties atomic.Uint64
	errs    atomic.Uint64
}

// NewSupervisor validates the config, builds the Materializer, and starts
// listening on the socket. Call Serve to handle requests and Close to shut down.
func NewSupervisor(cfg Config) (*Supervisor, error) {
	if cfg.SocketPath == "" || cfg.Manifest == nil {
		return nil, errors.New("shim: supervisor config missing required fields")
	}
	mat, err := NewMaterializer(cfg.Root, cfg.Manifest, cfg.Store, cfg.Table, cfg.BuildTime, cfg.Logger)
	if err != nil {
		return nil, err
	}

	// The socket's directory is the trust boundary (design §4): only this user
	// may connect. Force 0700 rather than trusting the caller's umask.
	sockDir := filepath.Dir(cfg.SocketPath)
	if err := os.Chmod(sockDir, 0o700); err != nil {
		return nil, fmt.Errorf("shim: restrict socket dir %q: %w", sockDir, err)
	}
	// Remove a stale socket from a previous run; refuse to clobber a non-socket.
	if fi, err := os.Lstat(cfg.SocketPath); err == nil {
		if fi.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("shim: socket path %q exists and is not a socket", cfg.SocketPath)
		}
		if err := os.Remove(cfg.SocketPath); err != nil {
			return nil, fmt.Errorf("shim: remove stale socket %q: %w", cfg.SocketPath, err)
		}
	}
	ln, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("shim: listen on %q: %w", cfg.SocketPath, err)
	}

	return &Supervisor{
		mat:           mat,
		chunkRequests: cfg.ChunkRequests,
		log:           logging.OrDefault(cfg.Logger),
		ln:            ln,
	}, nil
}

// SocketPath returns the address the supervisor is listening on.
func (s *Supervisor) SocketPath() string { return s.ln.Addr().String() }

// Root returns the symlink-resolved workspace root.
func (s *Supervisor) Root() string { return s.mat.Root() }

// Serve accepts and handles requests until Close. Returns nil on clean shutdown.
func (s *Supervisor) Serve() error {
	s.log.Info("shim supervisor listening", "socket", s.SocketPath(), "root", s.mat.Root())
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return nil
			}
			return fmt.Errorf("shim: accept: %w", err)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// Close stops the listener and waits for in-flight requests to finish.
func (s *Supervisor) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	err := s.ln.Close()
	s.wg.Wait()
	if err != nil {
		return fmt.Errorf("shim: close listener: %w", err)
	}
	return nil
}

// handleConn serves exactly one request (one request per connection keeps the
// C side trivial).
func (s *Supervisor) handleConn(conn net.Conn) {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(connTimeout)); err != nil {
		s.log.Warn("set connection deadline", "err", err)
		return
	}

	r := bufio.NewReaderSize(conn, 4096)
	line, err := readLine(r, maxLine)
	if err != nil {
		s.errs.Add(1)
		s.log.Warn("bad request", "err", err)
		s.reply(conn, "ERR "+sanitize(err.Error()))
		return
	}

	verb, arg, _ := strings.Cut(line, " ")
	switch verb {
	case verbEnsure:
		s.ensures.Add(1)
		rel, err := s.mat.RelPath(arg)
		if err == nil {
			err = s.mat.Ensure(rel)
		}
		if err != nil {
			s.errs.Add(1)
			s.log.Error("ensure failed", "path", arg, "err", err)
			s.reply(conn, "ERR "+sanitize(err.Error()))
			return
		}
		s.reply(conn, "OK")

	case verbDirty:
		s.dirties.Add(1)
		rel, err := s.mat.RelPath(arg)
		if err == nil {
			err = s.mat.Dirty(rel)
		}
		if err != nil {
			s.errs.Add(1)
			s.log.Error("dirty failed", "path", arg, "err", err)
			s.reply(conn, "ERR "+sanitize(err.Error()))
			return
		}
		s.reply(conn, "OK")

	case verbMaterializeAll:
		n, err := s.mat.MaterializeAll()
		if err != nil {
			s.errs.Add(1)
			s.log.Error("materialize-all failed", "materialized", n, "err", err)
			s.reply(conn, "ERR "+sanitize(err.Error()))
			return
		}
		s.reply(conn, fmt.Sprintf("OK files=%d", n))

	case verbStats:
		s.reply(conn, "OK "+s.statsLine())

	default:
		s.errs.Add(1)
		s.log.Warn("unknown verb", "verb", verb)
		s.reply(conn, "ERR unknown verb "+sanitize(verb))
	}
}

// statsLine renders the STATS payload.
func (s *Supervisor) statsLine() string {
	c := s.mat.Table().Counts()
	var chunkReqs uint64
	if s.chunkRequests != nil {
		chunkReqs = s.chunkRequests()
	}
	return fmt.Sprintf(
		"ensures=%d dirty=%d placeholder=%d materialized=%d local=%d torn=%d errors=%d chunk_requests=%d",
		s.ensures.Load(), s.dirties.Load(),
		c.Placeholder, c.Materialized, c.Local, c.Torn,
		s.errs.Load(), chunkReqs)
}

// reply writes one response line, logging (not failing) on error.
func (s *Supervisor) reply(conn net.Conn, line string) {
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		s.log.Warn("write reply", "err", err)
	}
}

// readLine reads one \n-terminated line of at most max bytes.
func readLine(r *bufio.Reader, max int) (string, error) {
	var b strings.Builder
	for {
		chunkBytes, err := r.ReadSlice('\n')
		b.Write(chunkBytes)
		if b.Len() > max {
			return "", fmt.Errorf("request line exceeds %d bytes", max)
		}
		switch err {
		case nil:
			return strings.TrimRight(b.String(), "\r\n"), nil
		case bufio.ErrBufferFull:
			continue
		default:
			return "", fmt.Errorf("read request: %w", err)
		}
	}
}

// sanitize keeps protocol replies single-line.
func sanitize(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}
