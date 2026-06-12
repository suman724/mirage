package shim

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/fsutil"
	"github.com/suman724/mirage/internal/logging"
)

// SkeletonResult summarizes one skeleton build.
type SkeletonResult struct {
	Files   int    // placeholders now present (created or pre-existing)
	Created int    // placeholders created by this build
	Bytes   uint64 // logical bytes the skeleton represents (apparent sizes)
}

// BuildSkeleton makes root a complete, metadata-true projection of the
// manifest: every parent directory exists and every file is present as a
// sparse placeholder with its true size and mode, stamped with buildTime
// (manifest mtime arrives in S4). Cost is O(files) metadata operations — no
// chunk is fetched. After this, stat/readdir/find/glob work natively for any
// binary with zero interception (design §3.1).
//
// The build is idempotent so a supervisor restart over a persistent workspace
// is safe: an existing file is never touched (its state — materialized,
// local, or torn — was replayed from the journal), only observed to record
// its pristine marker.
func BuildSkeleton(root string, m *chunk.Manifest, table *Table, buildTime time.Time, logger *slog.Logger) (SkeletonResult, error) {
	log := logging.OrDefault(logger)
	var res SkeletonResult
	start := time.Now()

	for _, f := range m.Files {
		size := entrySize(f)
		dst, err := fsutil.SafeJoin(root, f.Path)
		if err != nil {
			return res, fmt.Errorf("shim: skeleton: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return res, fmt.Errorf("shim: skeleton: create dirs for %q: %w", f.Path, err)
		}

		fi, err := os.Lstat(dst)
		switch {
		case err == nil:
			// Pre-existing (restart over a persistent workspace, or a tree
			// collision). Never touch its content; just sanity-check shape.
			if !fi.Mode().IsRegular() {
				return res, fmt.Errorf("shim: skeleton: %q exists and is not a regular file (%s)", f.Path, fi.Mode())
			}
		case os.IsNotExist(err):
			if err := createPlaceholder(dst, size, placeholderMode(f.Mode), buildTime); err != nil {
				return res, fmt.Errorf("shim: skeleton: placeholder %q: %w", f.Path, err)
			}
			res.Created++
		default:
			return res, fmt.Errorf("shim: skeleton: stat %q: %w", f.Path, err)
		}

		marker, err := markerOf(dst)
		if err != nil {
			return res, fmt.Errorf("shim: skeleton: fingerprint %q: %w", f.Path, err)
		}
		table.Track(f.Path, marker)
		res.Files++
		res.Bytes += uint64(size)
	}

	log.Info("skeleton built",
		"root", root, "files", res.Files, "created", res.Created,
		"logical_bytes", res.Bytes, "elapsed", time.Since(start))
	return res, nil
}

// createPlaceholder makes a sparse file of the given apparent size: created
// exclusively (a concurrent creation is a bug we want loud), truncated to
// size (allocating no blocks on any mainstream filesystem), and stamped with
// buildTime so the mtime survives materialization (the future git-status
// linchpin, design §6).
func createPlaceholder(dst string, size int64, mode os.FileMode, buildTime time.Time) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return fmt.Errorf("truncate to %d: %w", size, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Chtimes(dst, buildTime, buildTime); err != nil {
		return fmt.Errorf("set mtime: %w", err)
	}
	return nil
}

// placeholderMode maps a manifest mode to the placeholder's permission bits,
// defaulting like reconstruct mode does for manifests that carry none.
func placeholderMode(mode uint32) os.FileMode {
	m := os.FileMode(mode).Perm()
	if m == 0 {
		m = 0o644
	}
	return m
}

// markerOf fingerprints the file at path for the pristine-placeholder check.
func markerOf(path string) (Marker, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Marker{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Marker{}, fmt.Errorf("no syscall.Stat_t for %q (unsupported platform)", path)
	}
	return Marker{Ino: st.Ino, Size: fi.Size()}, nil
}

// isPristine reports whether the file at path is still the untouched sparse
// placeholder recorded by marker: same inode, same apparent size, zero
// allocated blocks (design §4.1 safeguard 1). Anything else — different
// inode (renamed-over), allocated blocks (written-to), different size —
// means the content is the user's, and must never be overwritten.
func isPristine(path string, marker Marker) (bool, string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, "", fmt.Errorf("no syscall.Stat_t for %q (unsupported platform)", path)
	}
	switch {
	case !fi.Mode().IsRegular():
		return false, fmt.Sprintf("not a regular file (%s)", fi.Mode()), nil
	case st.Ino != marker.Ino:
		return false, fmt.Sprintf("inode changed (%d -> %d): replaced (rename-over save?)", marker.Ino, st.Ino), nil
	case fi.Size() != marker.Size:
		return false, fmt.Sprintf("size changed (%d -> %d)", marker.Size, fi.Size()), nil
	case st.Blocks != 0:
		return false, fmt.Sprintf("%d blocks allocated: written in place", st.Blocks), nil
	}
	return true, "", nil
}

// entrySize is the logical file size implied by a manifest entry's chunks.
func entrySize(f chunk.FileEntry) int64 {
	var n int64
	for _, c := range f.Chunks {
		n += int64(c.Size)
	}
	return n
}
