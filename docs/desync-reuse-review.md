# desync reuse review (task 2.10)

**Principle:** prefer existing `folbricht/desync` capabilities over hand-rolled
code. This catalogues what desync v1.0.1 already provides against what we built
or planned, with a decision for each.

_Last updated: 2026-06-07._

## What desync already provides

| Capability | desync API | We currently | Verdict |
|---|---|---|---|
| Content-defined chunking + content hash | `NewChunker`, `Digest`, `NewChunk` | use it (`internal/chunk.Split`) | ✅ already reusing |
| Local disk cache fronting a slow store | `Cache` (`NewCache(s Store, l WriteStore)`) | hand-rolled `server/cache` | ⚠️ **replace with desync.Cache** |
| Single-flight (coalesce concurrent misses) | `DedupQueue` (`NewDedupQueue(store)`) | hand-rolled `singleflight` in `server/cache` | ⚠️ **replace with desync.DedupQueue** |
| Disk chunk store | `LocalStore` (`NewLocalStore`) | — | ✅ use as the cache's `WriteStore` |
| Concurrent blob assembly from index+store | `AssembleFile`, `Copy` | hand loop in `reconstruct` | ◻️ optional: adopt for concurrency/seeding |
| Hash verification on read | `NewChunkWithID(id, b, false)` | manual `HashOf(data)==h` in channelstore | ⚠️ free if channelstore returns desync `Chunk` |
| FUSE mount of an index (single blob) | `MountIndex`, `IndexMountFS`, `indexFileHandle` | not yet (task 2.5) | ✅ **reuse the per-file read machinery** |
| Directory archive (tree ⇄ blob) | `Tar`, `UnTar`, `UnTarIndex`, `FilesystemReader/Writer` | our JSON manifest | ◻️ decision needed (see below) |
| Store multiplexer / failover | `StoreRouter`, `FailoverGroup` | — | ◻️ not needed yet |

## The one blocker: interface shape

desync's reuse pieces (`Cache`, `DedupQueue`, `AssembleFile`, FUSE) are all
written against **`desync.Store`**:

```go
type Store interface {
    GetChunk(id ChunkID) (*Chunk, error)   // NOTE: no context.Context
    HasChunk(id ChunkID) (bool, error)
    io.Closer
    fmt.Stringer
}
```

Our `channelstore` implements our own ctx-aware `chunk.Store`
(`GetChunk(ctx, Hash) ([]byte, error)`). To reuse desync's machinery,
`channelstore` should **implement `desync.Store`** instead, holding the stream
context internally and applying the per-fetch timeout there (desync's own HTTP
store does exactly this — timeout lives in the store, not the call signature).
Cancellation still propagates: when the stream drops, the stored context is
done and in-flight fetches fail.

Returning a desync `*Chunk` via `NewChunkWithID(id, data, false)` also gives us
**hash verification for free**, deleting our manual check.

## Recommended changes

1. **`channelstore` implements `desync.Store`.** `GetChunk(id)` sends the
   `ChunkRequest`, awaits the reply (request_id correlation unchanged), and
   returns `desync.NewChunkWithID(id, data, false)`. `HasChunk` returns
   `true,nil` (the server only ever asks for published hashes). Keep the
   `Requests()` metric and the internal timeout.
2. **Delete `server/cache`.** Build the chain in `server/transport`:
   `desync.NewCache(desync.NewDedupQueue(channelStore), localStore)` where
   `localStore` is a `desync.LocalStore` in a temp cache dir. This gives us the
   cache **and** single-flight from the library.
3. **Reconstruction** reads `Chunk.Data()` from the chain (or adopt
   `AssembleFile` for concurrency — optional).
4. **Metrics:** keep `channelstore.Requests()` (= channel fetches = cache
   misses). `cache_hits = total_refs - Requests()`. desync.Cache exposes no
   counters, so derive hits rather than instrument the cache.

Net: deletes `server/cache` (~80 LOC) + manual verification, and reuses
battle-tested cache/dedup. Cost: `channelstore` adopts desync's
context-free `Store` signature (timeout/cancel handled internally).

## FUSE (task 2.5) — decision needed

desync's `IndexMountFS` mounts **one index as a single file** (a blob), not a
browsable directory tree. Two ways to present the workspace as a real POSIX
tree on the sandbox:

- **A. Per-file index + thin tree FUSE (recommended, matches design §4.2).**
  Keep our manifest (path → chunks), build a `desync.Index` per file, and write
  a thin FUSE tree whose file `Read` reuses desync's index-backed,
  read-at-offset logic (the `indexFileHandle` pattern) over the store chain.
  Lazy per-file faulting; secret exclusion stays at index-build. Most control.
- **B. Whole-tree catar + desync mount.** Client `Tar`s the workspace (with a
  filtering `FilesystemReader` for secrets) into a catar blob and indexes it;
  server mounts via desync. Maximum reuse, but the catar is a single blob —
  presenting it as a writable, browsable tree and keeping per-file laziness is
  awkward, and secret filtering moves into a Tar walker.

**Recommendation: A** — reuse desync's chunk store + per-file read machinery,
keep a thin custom tree layer. This is exactly what design §4.2 calls the
"preferred" approach.

## Decisions log

- ✅ Chunker/hash: already desync.
- ✅ Adopt `desync.Cache` + `desync.DedupQueue` + `LocalStore`; delete `server/cache`.
- ✅ `channelstore` implements `desync.Store`; hash-verify via `NewChunkWithID`.
- ◻️ Reconstruction: keep simple loop now; `AssembleFile` optional later.
- ✅ FUSE: option A (thin tree FUSE reusing desync per-file read machinery).
