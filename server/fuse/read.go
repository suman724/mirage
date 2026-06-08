// Package fuse presents a published workspace manifest as a browsable POSIX
// directory tree on the server (the cloud sandbox), faulting each file's bytes
// lazily over the channel on read (design §4.2). It is a thin custom tree layer
// over desync's index + Store: directory structure is ours, while per-file
// reads reuse desync chunk IDs and the store chain
// (Cache -> DedupQueue -> channelstore).
//
// This file holds the pure offset->chunk read logic, which is unit-testable
// without an actual kernel mount. The mount glue lives in mount.go.
package fuse

import (
	"fmt"

	"github.com/folbricht/desync"

	"github.com/suman724/mirage/internal/chunk"
)

// IndexFromRefs builds a desync.Index (the per-file chunk table with absolute
// offsets) from a manifest file entry's ordered chunk refs.
func IndexFromRefs(refs []chunk.Ref) desync.Index {
	idx := desync.Index{}
	idx.Index.ChunkSizeMin = chunk.ChunkMin
	idx.Index.ChunkSizeAvg = chunk.ChunkAvg
	idx.Index.ChunkSizeMax = chunk.ChunkMax
	var start uint64
	for _, r := range refs {
		idx.Chunks = append(idx.Chunks, desync.IndexChunk{
			ID:    desync.ChunkID(r.Hash),
			Start: start,
			Size:  uint64(r.Size),
		})
		start += uint64(r.Size)
	}
	return idx
}

// FileSize returns the logical size of a file from its chunk table.
func FileSize(idx desync.Index) int64 {
	if len(idx.Chunks) == 0 {
		return 0
	}
	last := idx.Chunks[len(idx.Chunks)-1]
	return int64(last.Start + last.Size)
}

// ReadRange fills dest with the file's bytes starting at off, faulting only the
// chunks that overlap the requested range through store. It returns the number
// of bytes written (which is short at EOF). This is exactly the lazy-read
// primitive a FUSE Read handler needs: a cold read faults its chunks over the
// channel; a warm read is served from the cache in the store chain.
func ReadRange(idx desync.Index, store desync.Store, dest []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("fuse: negative read offset %d", off)
	}
	if len(dest) == 0 {
		return 0, nil
	}
	end := off + int64(len(dest))
	n := 0
	for _, c := range idx.Chunks {
		cStart := int64(c.Start)
		cEnd := cStart + int64(c.Size)
		if cEnd <= off {
			continue // chunk entirely before the requested range
		}
		if cStart >= end {
			break // chunks are ordered; the rest are past the range
		}

		ck, err := store.GetChunk(c.ID)
		if err != nil {
			return n, fmt.Errorf("fuse: fault chunk %s: %w", c.ID, err)
		}
		data, err := ck.Data()
		if err != nil {
			return n, fmt.Errorf("fuse: decode chunk %s: %w", c.ID, err)
		}

		// Portion of this chunk that overlaps [off, end).
		from := int64(0)
		if off > cStart {
			from = off - cStart
		}
		to := int64(len(data))
		if end-cStart < to {
			to = end - cStart
		}
		if from < 0 || to > int64(len(data)) || from > to {
			return n, fmt.Errorf("fuse: chunk %s slice [%d:%d] invalid for %d bytes", c.ID, from, to, len(data))
		}
		destOff := cStart + from - off
		n += copy(dest[destOff:], data[from:to])
	}
	return n, nil
}
