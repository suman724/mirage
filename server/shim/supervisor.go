package shim

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/folbricht/desync"
	"golang.org/x/sync/singleflight"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/fsutil"
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

// Supervisor owns the ENSURE side of Shimmer: it listens on the unix socket
// and fills pristine placeholders with manifest content on demand, exactly
// once per path (per-path singleflight; chunk-level dedup already lives in
// the store chain). It is safe for concurrent use.
type Supervisor struct {
	root          string // symlink-resolved absolute workspace root
	files         map[string]chunk.FileEntry
	store         desync.Store
	table         *Table
	buildTime     time.Time
	chunkRequests func() uint64
	log           *slog.Logger

	ln net.Listener
	sf singleflight.Group
	wg sync.WaitGroup

	closed  atomic.Bool
	ensures atomic.Uint64
	dirties atomic.Uint64
	errs    atomic.Uint64
}

// NewSupervisor validates the config and starts listening on the socket. Call
// Serve to begin handling requests and Close to shut down.
func NewSupervisor(cfg Config) (*Supervisor, error) {
	if cfg.Root == "" || cfg.SocketPath == "" || cfg.Manifest == nil || cfg.Store == nil || cfg.Table == nil {
		return nil, errors.New("shim: supervisor config missing required fields")
	}

	// Resolve symlinks so prefix checks agree with canonicalized client paths
	// (the C shim sends realpath() output; macOS /tmp is itself a symlink).
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("shim: resolve root %q: %w", cfg.Root, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("shim: resolve root %q: %w", cfg.Root, err)
	}

	files := make(map[string]chunk.FileEntry, len(cfg.Manifest.Files))
	for _, f := range cfg.Manifest.Files {
		files[f.Path] = f
	}

	// The socket's directory is the trust boundary (design §4): only this
	// user may connect. Force 0700 rather than trusting the caller's umask.
	sockDir := filepath.Dir(cfg.SocketPath)
	if err := os.Chmod(sockDir, 0o700); err != nil {
		return nil, fmt.Errorf("shim: restrict socket dir %q: %w", sockDir, err)
	}
	// Remove a stale socket from a previous run; refuse to clobber anything
	// that is not a socket.
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
		root:          root,
		files:         files,
		store:         cfg.Store,
		table:         cfg.Table,
		buildTime:     cfg.BuildTime,
		chunkRequests: cfg.ChunkRequests,
		log:           logging.OrDefault(cfg.Logger),
		ln:            ln,
	}, nil
}

// SocketPath returns the address the supervisor is listening on.
func (s *Supervisor) SocketPath() string { return s.ln.Addr().String() }

// Root returns the symlink-resolved workspace root.
func (s *Supervisor) Root() string { return s.root }

// Serve accepts and handles requests until Close. It returns nil on a clean
// shutdown.
func (s *Supervisor) Serve() error {
	s.log.Info("shim supervisor listening", "socket", s.SocketPath(), "root", s.root, "files", len(s.files))
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

// handleConn serves exactly one request (design §4: one request per
// connection keeps the C side trivial).
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
		rel, err := s.relPath(arg)
		if err == nil {
			err = s.Ensure(rel)
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
		rel, err := s.relPath(arg)
		if err == nil {
			err = s.dirty(rel)
		}
		if err != nil {
			s.errs.Add(1)
			s.log.Error("dirty failed", "path", arg, "err", err)
			s.reply(conn, "ERR "+sanitize(err.Error()))
			return
		}
		s.reply(conn, "OK")

	case verbMaterializeAll:
		n, err := s.MaterializeAll()
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

// Ensure guarantees the file at the manifest-relative path holds real,
// correct content before the caller opens it: a placeholder is filled from
// the store; materialized and local files pass through untouched; an
// untracked path is a no-op (a tool-created local file, or a path that does
// not exist — the caller's real open() gives the right answer either way).
func (s *Supervisor) Ensure(rel string) error {
	state, tracked := s.table.Get(rel)
	if !tracked {
		s.log.Debug("ensure for untracked path (local file or nonexistent)", "path", rel)
		return nil
	}
	if state == StateMaterialized || state == StateLocal {
		return nil
	}
	// Collapse concurrent ENSUREs of one path into a single fill. (Chunk-level
	// dedup across files is the store chain's job; this is file-level.)
	_, err, _ := s.sf.Do(rel, func() (any, error) {
		return nil, s.materialize(rel)
	})
	return err
}

// MaterializeAll fills every placeholder (exec-gate fallback / --out
// degradation, design §5). It fails fast on the first error, returning how
// many files are real either way.
func (s *Supervisor) MaterializeAll() (int, error) {
	paths := make([]string, 0, len(s.files))
	for rel := range s.files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	n := 0
	for _, rel := range paths {
		if err := s.Ensure(rel); err != nil {
			return n, fmt.Errorf("shim: materialize all at %q: %w", rel, err)
		}
		n++
	}
	return n, nil
}

// dirty flips a path to local (content no longer described by the manifest).
// Tool-created files outside the manifest are tracked too, so a future
// write-back has the complete (lower-bound, §3.2) change set.
func (s *Supervisor) dirty(rel string) error {
	if err := s.table.SetLocal(rel); err != nil {
		return err
	}
	s.log.Info("path marked local (diverged from manifest)", "path", rel)
	return nil
}

// materialize fills one placeholder in place. Caller holds the per-path
// singleflight slot.
func (s *Supervisor) materialize(rel string) error {
	// Re-check under the flight: a queued duplicate must not re-fill.
	state, tracked := s.table.Get(rel)
	if !tracked || state == StateMaterialized || state == StateLocal {
		return nil
	}

	fe, ok := s.files[rel]
	if !ok {
		return fmt.Errorf("shim: %q tracked as placeholder but absent from manifest", rel)
	}
	abs, err := fsutil.SafeJoin(s.root, rel)
	if err != nil {
		return fmt.Errorf("shim: materialize: %w", err)
	}
	size := entrySize(fe)

	// Pristine-placeholder check (design §4.1 safeguard 1): if anything
	// replaced or wrote the placeholder out from under us (rename-over
	// atomic save being the common case), the on-disk bytes are the user's —
	// flip to local and NEVER overwrite. A torn path (crash mid-fill) is
	// known to hold half-written manifest content, so it skips the check and
	// is re-filled.
	if !s.table.IsTorn(rel) {
		marker, ok := s.table.MarkerFor(rel)
		if !ok {
			return fmt.Errorf("shim: no pristine marker for %q", rel)
		}
		pristine, reason, err := isPristine(abs, marker)
		if os.IsNotExist(err) {
			// Placeholder gone: a tool removed/renamed it away. Its absence
			// is the user's state; preserve it.
			s.log.Warn("placeholder disappeared; treating as local deletion", "path", rel)
			return s.table.SetLocal(rel)
		}
		if err != nil {
			return fmt.Errorf("shim: pristine check %q: %w", rel, err)
		}
		if !pristine {
			s.log.Warn("placeholder no longer pristine; preserving on-disk content as local",
				"path", rel, "reason", reason)
			return s.table.SetLocal(rel)
		}
	}

	// Durable intent first: if we die mid-fill, replay sees `materializing`
	// (torn) and re-fills instead of mistaking half-written content for a
	// user edit. If this append fails we must not touch the file.
	if err := s.table.SetMaterializing(rel); err != nil {
		return fmt.Errorf("shim: journal fill intent for %q: %w", rel, err)
	}

	start := time.Now()
	if err := s.fill(abs, fe, size); err != nil {
		// Journal stays at `materializing`: the next ENSURE (or a restart)
		// retries the fill via the torn path. Mark it torn now for in-process
		// retries too.
		s.errs.Add(1)
		s.table.MarkTorn(rel)
		return fmt.Errorf("shim: fill %q: %w", rel, err)
	}

	// Re-stamp the skeleton mtime so materialization is mtime-invisible (the
	// future git-status property, design §6). Content is already correct, so
	// failure here is loud but non-fatal.
	if err := os.Chtimes(abs, s.buildTime, s.buildTime); err != nil {
		s.log.Warn("restore placeholder mtime", "path", rel, "err", err)
	}

	if err := s.table.SetMaterialized(rel); err != nil {
		// Content is correct; only restart economy is affected (a redundant
		// re-fill). Report it loudly but do not fail the ENSURE.
		s.log.Error("journal materialized state", "path", rel, "err", err)
	}
	s.log.Info("materialized", "path", rel, "bytes", size,
		"chunks", len(fe.Chunks), "elapsed", time.Since(start))
	return nil
}

// fill streams the file's chunks from the store into the placeholder in
// place. The placeholder already has the exact final size, so a successful
// sequential write of every chunk reproduces the manifest content exactly.
func (s *Supervisor) fill(abs string, fe chunk.FileEntry, size int64) error {
	f, err := os.OpenFile(abs, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open for fill: %w", err)
	}
	var written int64
	for _, ref := range fe.Chunks {
		ck, err := s.store.GetChunk(desync.ChunkID(ref.Hash))
		if err != nil {
			f.Close()
			return fmt.Errorf("fault chunk %s: %w", ref.Hash, err)
		}
		data, err := ck.Data()
		if err != nil {
			f.Close()
			return fmt.Errorf("decode chunk %s: %w", ref.Hash, err)
		}
		n, err := f.Write(data)
		written += int64(n)
		if err != nil {
			f.Close()
			return fmt.Errorf("write at %d: %w", written, err)
		}
	}
	if written != size {
		f.Close()
		return fmt.Errorf("wrote %d bytes, manifest says %d", written, size)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// relPath maps an absolute, canonicalized client path to a manifest-relative
// slash path, rejecting anything outside the workspace root.
func (s *Supervisor) relPath(abs string) (string, error) {
	if abs == "" {
		return "", errors.New("shim: empty path")
	}
	if !filepath.IsAbs(abs) {
		return "", fmt.Errorf("shim: path %q is not absolute", abs)
	}
	clean := filepath.Clean(abs)
	if clean == s.root {
		return "", fmt.Errorf("shim: path %q is the workspace root", abs)
	}
	if !strings.HasPrefix(clean, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("shim: path %q is outside workspace root %q", abs, s.root)
	}
	rel, err := filepath.Rel(s.root, clean)
	if err != nil {
		return "", fmt.Errorf("shim: relativize %q: %w", abs, err)
	}
	return filepath.ToSlash(rel), nil
}

// statsLine renders the STATS payload.
func (s *Supervisor) statsLine() string {
	c := s.table.Counts()
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

// reply writes one response line, logging (not failing) on error — the
// client may have timed out and gone away.
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
