# Mirage — Task Tracker

Milestone: **minimal end-to-end data-transfer validation** (no TUI, no FUSE).
Status legend: ✅ done · 🚧 in progress · ⬜ not started.

_Last updated: 2026-06-07._

## Definition of Done (milestone gate)

- ✅ `go build ./...` succeeds.
- ✅ Server + client on localhost reconstruct the multi-file test tree
  byte-for-byte using **only** chunks transferred over the stream.
- ✅ Integration test passes (`go test ./...`).
- ⬜ Commit it. _(awaiting go-ahead to commit)_

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

## Next iterations (NOT started — do not begin yet)

- ⬜ Server-side FUSE mount so a real POSIX read faults chunks via `ChunkRequest`.
- ⬜ Minimal non-interactive CLI to drive it.
- ⬜ TUI (last).
- ⬜ Swap placeholder chunker for `folbricht/desync` behind the existing seam.
