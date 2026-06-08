package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"syscall"

	"github.com/folbricht/desync"
	gofusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/logging"
)

// Mount mounts the manifest as a read-only directory tree at mountpoint,
// faulting file contents lazily through store. The returned *fuse.Server is
// live; call Unmount (or Wait) on it to tear the mount down. Mounting requires
// a FUSE kernel module (macFUSE on macOS, /dev/fuse on Linux) at runtime.
func Mount(mountpoint string, m *chunk.Manifest, store desync.Store, logger *slog.Logger) (*fuse.Server, error) {
	log := logging.OrDefault(logger)
	root := &dirRoot{manifest: m, store: store, log: log}
	server, err := gofusefs.Mount(mountpoint, root, &gofusefs.Options{
		MountOptions: fuse.MountOptions{
			FsName: "mirage",
			Name:   "mirage",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("fuse: mount %q: %w", mountpoint, err)
	}
	log.Info("workspace mounted", "mountpoint", mountpoint, "files", len(m.Files))
	return server, nil
}

// dirRoot is the FUSE root inode. It builds the static directory tree from the
// manifest at mount time; file leaves fault their bytes lazily on read.
type dirRoot struct {
	gofusefs.Inode
	manifest *chunk.Manifest
	store    desync.Store
	log      *slog.Logger
}

var _ gofusefs.NodeOnAdder = (*dirRoot)(nil)

// OnAdd builds the directory hierarchy and file leaves from the manifest.
func (r *dirRoot) OnAdd(ctx context.Context) {
	for _, f := range r.manifest.Files {
		parts := strings.Split(f.Path, "/")
		parent := &r.Inode

		// Create (or descend into) intermediate directories.
		for _, comp := range parts[:len(parts)-1] {
			child := parent.GetChild(comp)
			if child == nil {
				child = parent.NewPersistentInode(ctx, &gofusefs.Inode{},
					gofusefs.StableAttr{Mode: fuse.S_IFDIR})
				parent.AddChild(comp, child, true)
			}
			parent = child
		}

		idx := IndexFromRefs(f.Chunks)
		leaf := &fileNode{
			idx:   idx,
			store: r.store,
			size:  FileSize(idx),
			mode:  f.Mode,
			log:   r.log,
		}
		name := parts[len(parts)-1]
		parent.AddChild(name, parent.NewPersistentInode(ctx, leaf,
			gofusefs.StableAttr{Mode: fuse.S_IFREG}), true)
	}
}

// fileNode is a lazily-faulted file: its bytes are fetched from the store chain
// on read, never held eagerly.
type fileNode struct {
	gofusefs.Inode
	idx   desync.Index
	store desync.Store
	size  int64
	mode  uint32
	log   *slog.Logger
}

var (
	_ gofusefs.NodeOpener    = (*fileNode)(nil)
	_ gofusefs.NodeGetattrer = (*fileNode)(nil)
	_ gofusefs.NodeReader    = (*fileNode)(nil)
)

func (n *fileNode) Getattr(ctx context.Context, fh gofusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	mode := n.mode & 0o777
	if mode == 0 {
		mode = 0o644
	}
	out.Mode = mode
	out.Size = uint64(n.size)
	return 0
}

func (n *fileNode) Open(ctx context.Context, flags uint32) (gofusefs.FileHandle, uint32, syscall.Errno) {
	// Read-only; no per-handle state needed. KEEP_CACHE lets the kernel cache
	// pages so warm re-reads don't even reach us.
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *fileNode) Read(ctx context.Context, fh gofusefs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	got, err := ReadRange(n.idx, n.store, dest, off)
	if err != nil {
		n.log.Error("read fault failed", "offset", off, "len", len(dest), "err", err)
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:got]), 0
}
