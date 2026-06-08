# Mirage — Task Tracker

Milestone: **minimal end-to-end data-transfer validation** (no TUI, no FUSE).
Status legend: ✅ done · 🚧 in progress · ⬜ not started.

_Last updated: 2026-06-07._

## Definition of Done (milestone gate)

- ✅ `go build ./...` succeeds.
- ✅ Server + client on localhost reconstruct the multi-file test tree
  byte-for-byte using **only** chunks transferred over the stream.
- ✅ Integration test passes (`go test ./...`).
- ✅ Commit it. _(commit `4dffa96`)_

## Core tasks (from the iteration brief)

| # | Task | Status | Notes |
|---|------|--------|-------|
| 1 | Scaffold: `git init`, `go.mod` (module `github.com/suman724/mirage`, go 1.23), `.gitignore` (Go + .DS_Store + IDE), README → HANDOFF | ✅ | `git init` done; module + go 1.23; deps pinned to build on 1.23. |
| 2 | Proto codegen: fix `go_package`, install buf, add `buf.yaml`/`buf.gen.yaml`, generate into `proto/mirage/v1/` | ✅ | `go_package` → `github.com/suman724/mirage/...`; buf via Homebrew; plugins via `go install`; `.proto` moved to `proto/mirage/v1/` so `source_relative` output matches the package path. Generated code committed. |
| 3 | Chunking via desync (Store interface), or a placeholder SHA-256 chunker behind a `GetChunk(hash)->bytes` seam | ✅ | **Placeholder** fixed-size (64 KiB) SHA-256 chunker in `internal/chunk` behind the `chunk.Store` seam. Swap to `folbricht/desync` later stays local. |
| 4 | `server/` binary: gRPC `Mirage.Connect`; on Hello+IndexPublish drive the protocol, send ChunkRequests, write reconstructed files to `--out`; `server/channelstore` as the Store whose `GetChunk` sends a ChunkRequest and awaits the response (correlate by `request_id`) | ✅ | `server/transport` + `server/channelstore` + `server/main.go`. Hash-verifies each served chunk; reports a `Result`. |
| 5 | `client/` binary: dial the server, send Hello + IndexPublish for `--dir`, answer ChunkRequests from a local store built at index time; reject hashes absent from the published index | ✅ | `client/transport` (only Dialer) + `client/index` + `client/chunkstore` + `client/main.go`. Rejection enforced in `answerChunkRequest`. |
| 6 | Integration test: client at `testdata/`, reconstruct into temp dir, assert byte-identical trees, assert server got data ONLY via ChunkRequest | ✅ | `test/integration_test.go` over real localhost gRPC. Asserts byte-identical tree, `ChunkRequests > 0`, and secrets never reconstructed. |

## Additional requests (from the user)

| Task | Status | Notes |
|------|--------|-------|
| Clean `Makefile` | ✅ | Targets: `build test integration proto tools vet fmt tidy run-server run-client clean help`. Pins `GOTOOLCHAIN=local`, wires the plugin `PATH`. |
| Task-tracking doc with per-task status | ✅ | This file. |
| Unit tests; keep tests compiling as we progress | ✅ | Tests for `internal/chunk`, `client/chunkstore`, `client/index`, `server/channelstore`. All green. |
| Run integration tests | ✅ | `go test ./...` green; manual two-binary localhost run verified (6 files, 9 chunk requests; only secrets differ in `diff -r`). |
| `README.md` | ✅ | Overview, layout, quickstart, manual demo. |
| `CLAUDE.md` | ✅ | Iron rules, build/test/gen, code map, protocol flow. |

## Verification snapshot

- `go vet ./...` — clean.
- `go test ./...` — all packages pass (chunk, chunkstore, index, channelstore, test).
- Manual: `mirage-server` + `mirage-client` on `127.0.0.1` → reconstructed
  `testdata/workspace` byte-for-byte via chunk requests; `.env` and `id_rsa`
  correctly excluded (never published, never reconstructed).

---

# Iteration 2 — Lazy FUSE read (recommended path)

Goal of this iteration (design §9 Phase 2 / HANDOFF M2): a real POSIX `read()`
on the server faults chunks over the channel via `ChunkRequest`, a local cache
makes re-reads free, and reads are **lazy** — only touched files fault.

**Why this order (desync → cache → FUSE → harness):** desync is the foundation
that (a) gives real content-defined chunking + dedup, (b) provides the local
cache **Store chaining** that FUSE needs, and (c) drives **concurrent**
`GetChunk` calls — which our single gRPC stream already multiplexes via
`request_id`. desync is fully validatable on macOS; FUSE has a platform
dependency (macFUSE) we confront only after the foundation is solid.

> **On concurrency / multiple connections:** desync provides concurrent chunk
> *fetching* (worker-pool assembler) — we keep `channelstore` correct under
> concurrent `GetChunk` via `request_id` correlation, and add a test for it.
> Multiple *connections* / reconnect / multi-session is NOT desync's concern and
> is NOT needed for this milestone — explicitly deferred.

## Risks to validate EARLY (before building on them)

| Risk | Validation step | Fallback |
|---|---|---|
| desync deps may require Go > 1.23 (we're pinned; grpc 1.72.2) | `go get` desync in a throwaway build, run `go build`; check toolchain | pin an older desync tag; or raise module Go consciously |
| desync tree model: per-file `.caibx` vs catar+`.caidx` | small spike both ways; pick the simpler that preserves secret-exclusion at index time | keep our Manifest, embed desync per-file indexes |
| macFUSE not installed / needs kext approval on this Mac | `make doctor` checks for macFUSE; try a no-op mount early | validate FUSE in a Linux container; keep reconstruct path as the macOS-testable proof |
| Cold-read latency = 1 round-trip per missed chunk | measure round-trip in a benchmark; confirm cache hit path is local | prefetch (later phase) |

## Tasks

| # | Task | Status | Notes |
|---|------|--------|-------|
| 2.1 | Validate desync compatibility / resolve toolchain | ✅ | desync v1.0.1 + modern grpc need Go ≥1.25; **module raised to Go 1.25** (user-approved). Validated: desync v1.0.1 + grpc v1.81.1 + protobuf v1.36.11 build/run on go1.25. |
| 2.2 | Adopt desync behind the existing `chunk.Store` seam — real CDC + content hashing; keep secret-exclusion at index-build time | ✅ | `internal/chunk.Split` now uses `desync.NewChunker` (CDC, 16K/64K/256K) and `desync.Digest` chunk IDs. Transports/stores untouched. All tests + manual e2e green; dedup verified. |
| 2.3 | Server local **cache** store (cache-store chaining: `cache → channelstore`); re-reads served locally | ✅ | **Now via desync (see 2.10):** `desync.NewCache(desync.NewDedupQueue(channelstore), LocalStore)`. The hand-rolled `server/cache` was deleted. `Result` gains `TotalRefs`/`CacheHits` (hits derived = refs − channel fetches). Manual e2e: 9 refs → 8 channel fetches + 1 cache hit (the duplicate file). |
| 2.4 | Concurrency test: many concurrent `GetChunk` over one stream stay correct | ✅ | `TestGetChunkConcurrent` (200 interleaved/reordered responses) + integration asserts `ChunkRequests == unique`. Whole suite passes under `-race`. Proves `request_id` multiplexing. |
| 2.5 | FUSE mount (`hanwen/go-fuse`) on the server, backed by the desync store chain; read faults chunks lazily | ✅ (code) | `server/fuse`: `ReadRange` (pure offset→chunk lazy-read primitive, fully unit-tested incl. "1-byte read faults 1 chunk") + thin tree mount (`Mount`, dir/file inodes) reusing desync IDs + store chain. Compiles on macOS; **live mount deferred** to 2.5-val. |
| 2.5-val | **Live FUSE validation in Docker/Linux** (revisit when Docker is running) | ⬜ | Authored and ready: `Dockerfile` (golang:1.25 + fuse3) + `make fuse-validate` (runs with `/dev/fuse` + `--cap-add SYS_ADMIN`). `TestLiveMount` skips locally (no macFUSE) and runs in the container. Docker IS installed here (daemon currently off) — start it and run `make fuse-validate`. |
| 2.6 | Harness stub: a process that `cat`s a file on the mount; assert correct bytes via faults, and a 2nd read hits cache | ⬜ | Non-interactive driver; the M2 "done when". Depends on 2.5-val (needs a live mount). |
| 2.7 | Production hardening pass: structured logging (`log/slog`), context-aware errors, graceful shutdown, timeouts on every fetch | ✅ | `internal/logging` (slog setup, level/format flags, nil-safe injection). Transports + channelstore log lifecycle/chunk events; errors wrapped with context; per-fetch timeout (`DefaultFetchTimeout`); server `GracefulStop` on SIGINT/SIGTERM; client cancels on signal. Remains the standard for new code. |
| 2.8 | Keep README + CLAUDE.md + this file updated at each step | ⬜ | README gets a logical "how it works / how to use / who benefits" walkthrough. |
| 2.9 | After the iteration's features are done, write a **plain-language tech doc** explaining everything we built, in logical order | ✅ | [`docs/how-mirage-works.md`](docs/how-mirage-works.md): first-principles tour (content addressing, chunks, CDC, the manifest, the Store, the outbound-only connection, end-to-end flow, security, caching/dedup/single-flight, FUSE) for a reader with no desync/chunking/concurrency background. Maps every concept to its package; includes a "try it" section. |
| 2.10 | **Reuse-over-reinvent review** of desync (and other deps): catalogue what they already provide and replace our hand-rolled equivalents where it fits | ✅ | Review written up in [`docs/desync-reuse-review.md`](docs/desync-reuse-review.md). **Acted on:** `channelstore` now implements `desync.Store`; adopted `desync.Cache` + `DedupQueue` + `LocalStore` (deleted `server/cache`); hash verification via `desync.NewChunkWithID` (deleted manual check). FUSE (2.5) decided: option A — thin tree FUSE reusing desync's per-file read machinery. All green under `-race`. |

**Done when:** a stub harness reads `workspace/foo` on the FUSE mount, gets the
correct bytes via on-demand `ChunkRequest`s, and a second read is served from
local cache with zero new channel traffic — all with production-grade logging
and error handling, validated at every step.

## Engineering standard (non-negotiable, applies to ALL tasks)

- Structured logging (`log/slog`) with levels; no bare `log.Printf` in library code.
- Every error wrapped with context (`%w` + package prefix); no swallowed errors.
- Every network/fetch path is context-aware with a timeout; graceful shutdown.
- Unit tests beside each package; integration test stays green at every commit.
- `go build/vet/test ./...` and `gofmt -l .` clean before each step is "done".

## Later iterations (NOT started)

- ⬜ git fast-path (partial clone + working-tree delta).
- ⬜ Write-back (sandbox → client) with base-hash conflict detection.
- ⬜ Server-side search index (the search fault-storm mitigation).
- ⬜ Prefetch + warm snapshots.
- ⬜ TLS / auth / connection-bound tokens.
- ⬜ Multiple/concurrent connections + reconnect/resume (transport hardening).
- ⬜ TUI (last).
