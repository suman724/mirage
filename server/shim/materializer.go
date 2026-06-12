package shim

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/folbricht/desync"
	"golang.org/x/sync/singleflight"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/fsutil"
	"github.com/suman724/mirage/internal/logging"
)

// Materializer is the mechanism-agnostic core of Shimmer: it fills pristine
// placeholders with manifest content on demand, exactly once per path
// (per-path singleflight; chunk-level dedup lives in the store chain). It does
// not care how a request arrives — the socket Supervisor (S2 LD_PRELOAD shim)
// and the seccomp notification loop (S3′) are both thin front-ends over the
// same Materializer. Safe for concurrent use.
type Materializer struct {
	root      string // symlink-resolved absolute workspace root
	files     map[string]chunk.FileEntry
	store     desync.Store
	table     *Table
	buildTime time.Time
	log       *slog.Logger
	sf        singleflight.Group
}

// NewMaterializer resolves the workspace root (symlinks included, so prefix
// checks agree with canonicalized client paths) and indexes the manifest.
func NewMaterializer(root string, m *chunk.Manifest, store desync.Store, table *Table, buildTime time.Time, logger *slog.Logger) (*Materializer, error) {
	if root == "" || m == nil || store == nil || table == nil {
		return nil, errors.New("shim: materializer missing required fields")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("shim: resolve root %q: %w", root, err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("shim: resolve root %q: %w", root, err)
	}
	files := make(map[string]chunk.FileEntry, len(m.Files))
	for _, f := range m.Files {
		files[f.Path] = f
	}
	return &Materializer{
		root:      abs,
		files:     files,
		store:     store,
		table:     table,
		buildTime: buildTime,
		log:       logging.OrDefault(logger),
	}, nil
}

// Root returns the symlink-resolved workspace root.
func (m *Materializer) Root() string { return m.root }

// Table returns the underlying state table (shared with front-ends for STATS).
func (m *Materializer) Table() *Table { return m.table }

// Ensure guarantees the file at the manifest-relative path holds real, correct
// content before the caller opens it: a placeholder is filled from the store;
// materialized and local files pass through untouched; an untracked path is a
// no-op (a tool-created local file, or a path that does not exist — the
// caller's real open() gives the right answer either way).
func (m *Materializer) Ensure(rel string) error {
	state, tracked := m.table.Get(rel)
	if !tracked {
		m.log.Debug("ensure for untracked path (local file or nonexistent)", "path", rel)
		return nil
	}
	if state == StateMaterialized || state == StateLocal {
		return nil
	}
	// Collapse concurrent ENSUREs of one path into a single fill.
	_, err, _ := m.sf.Do(rel, func() (any, error) {
		return nil, m.materialize(rel)
	})
	return err
}

// MaterializeAll fills every placeholder (the full-sync escape hatch). Fails
// fast on the first error, returning how many files are real either way.
func (m *Materializer) MaterializeAll() (int, error) {
	paths := make([]string, 0, len(m.files))
	for rel := range m.files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	n := 0
	for _, rel := range paths {
		if err := m.Ensure(rel); err != nil {
			return n, fmt.Errorf("shim: materialize all at %q: %w", rel, err)
		}
		n++
	}
	return n, nil
}

// Dirty flips a path to local (content no longer described by the manifest).
// Tool-created files outside the manifest are tracked too, so a future
// write-back has the complete (lower-bound, §3.2) change set.
func (m *Materializer) Dirty(rel string) error {
	if err := m.table.SetLocal(rel); err != nil {
		return err
	}
	m.log.Info("path marked local (diverged from manifest)", "path", rel)
	return nil
}

// RelPath maps an absolute, canonicalized path to a manifest-relative slash
// path, rejecting anything outside the workspace root.
func (m *Materializer) RelPath(abs string) (string, error) {
	if abs == "" {
		return "", errors.New("shim: empty path")
	}
	if !filepath.IsAbs(abs) {
		return "", fmt.Errorf("shim: path %q is not absolute", abs)
	}
	clean := filepath.Clean(abs)
	if clean == m.root {
		return "", fmt.Errorf("shim: path %q is the workspace root", abs)
	}
	if !strings.HasPrefix(clean, m.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("shim: path %q is outside workspace root %q", abs, m.root)
	}
	rel, err := filepath.Rel(m.root, clean)
	if err != nil {
		return "", fmt.Errorf("shim: relativize %q: %w", abs, err)
	}
	return filepath.ToSlash(rel), nil
}

// materialize fills one placeholder in place. Caller holds the per-path
// singleflight slot.
func (m *Materializer) materialize(rel string) error {
	// Re-check under the flight: a queued duplicate must not re-fill.
	state, tracked := m.table.Get(rel)
	if !tracked || state == StateMaterialized || state == StateLocal {
		return nil
	}

	fe, ok := m.files[rel]
	if !ok {
		return fmt.Errorf("shim: %q tracked as placeholder but absent from manifest", rel)
	}
	abs, err := fsutil.SafeJoin(m.root, rel)
	if err != nil {
		return fmt.Errorf("shim: materialize: %w", err)
	}
	size := entrySize(fe)

	// Pristine-placeholder check (design §4.1 safeguard 1): if anything
	// replaced or wrote the placeholder out from under us (rename-over atomic
	// save being the common case), the on-disk bytes are the user's — flip to
	// local and NEVER overwrite. A torn path (crash mid-fill) is known to hold
	// half-written manifest content, so it skips the check and is re-filled.
	if !m.table.IsTorn(rel) {
		marker, ok := m.table.MarkerFor(rel)
		if !ok {
			return fmt.Errorf("shim: no pristine marker for %q", rel)
		}
		pristine, reason, err := isPristine(abs, marker)
		if os.IsNotExist(err) {
			m.log.Warn("placeholder disappeared; treating as local deletion", "path", rel)
			return m.table.SetLocal(rel)
		}
		if err != nil {
			return fmt.Errorf("shim: pristine check %q: %w", rel, err)
		}
		if !pristine {
			m.log.Warn("placeholder no longer pristine; preserving on-disk content as local",
				"path", rel, "reason", reason)
			return m.table.SetLocal(rel)
		}
	}

	// Durable intent first: if we die mid-fill, replay sees `materializing`
	// (torn) and re-fills instead of mistaking half-written content for a user
	// edit. If this append fails we must not touch the file.
	if err := m.table.SetMaterializing(rel); err != nil {
		return fmt.Errorf("shim: journal fill intent for %q: %w", rel, err)
	}

	start := time.Now()
	if err := m.fill(abs, fe, size); err != nil {
		// Journal stays at `materializing`: the next ENSURE (or a restart)
		// retries the fill via the torn path.
		m.table.MarkTorn(rel)
		return fmt.Errorf("shim: fill %q: %w", rel, err)
	}

	// Re-stamp the skeleton mtime so materialization is mtime-invisible.
	if err := os.Chtimes(abs, m.buildTime, m.buildTime); err != nil {
		m.log.Warn("restore placeholder mtime", "path", rel, "err", err)
	}

	if err := m.table.SetMaterialized(rel); err != nil {
		// Content is correct; only restart economy is affected.
		m.log.Error("journal materialized state", "path", rel, "err", err)
	}
	m.log.Info("materialized", "path", rel, "bytes", size,
		"chunks", len(fe.Chunks), "elapsed", time.Since(start))
	return nil
}

// fill streams the file's chunks from the store into the placeholder in place.
func (m *Materializer) fill(abs string, fe chunk.FileEntry, size int64) error {
	f, err := os.OpenFile(abs, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open for fill: %w", err)
	}
	var written int64
	for _, ref := range fe.Chunks {
		ck, err := m.store.GetChunk(desync.ChunkID(ref.Hash))
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
