# CLAUDE.md — working notes for Mirage

Orientation for an agent picking this up. Read `HANDOFF.md` and
`docs/workspace-fs-and-transport.md` first; this file is the practical layer on
top of them.

## What this repo is

A monorepo holding **both** sides of Mirage: a thin **client** (user's Win/Mac
machine) and a **server** (cloud Linux sandbox). The sandbox reads the user's
workspace as if local; files are content-addressed chunks faulted lazily over
**one connection the client dials out**. The server never dials in — it
originates requests *down the open stream*.

## Iron rules (do not violate)

- **Connection is ALWAYS client→server.** Only `client/transport` may `Dial`.
  `server/transport` only ever `Accept`s, then drives over the open stream. If
  you find yourself adding a Dial to the server, stop.
- **Protocol is chunk-HASH based, not path based.** The server can only request
  hashes the client published. The client **rejects** any hash absent from the
  published index (`client/transport.answerChunkRequest`). This is the security
  boundary (design §6) — keep it intact.
- **Secret exclusion happens at index-build time** on the client
  (`client/index.IsSecret` + excluded dirs). Secret files are never chunked, so
  their chunks can never be requested. Don't move this enforcement server-side.
- **Reuse desync; don't reinvent.** Before hand-rolling a primitive (cache,
  dedup, assembly, FUSE, archive), check desync's API first — it already
  provides most of it. Server components speak **`desync.Store`**
  (`GetChunk(id) (*Chunk, error)`); the chunk store chain is
  `Cache(DedupQueue(channelstore), LocalStore)`. See `docs/desync-reuse-review.md`.

## Build / test / generate

```bash
make build        # bin/mirage-server, bin/mirage-client
make test         # unit + integration (go test ./...)
make integration  # just the end-to-end test, verbose
make proto        # regen Go from proto/ (buf); `make tools` installs plugins
make vet fmt tidy
```

- **Go toolchain:** module is `go 1.25` (raised from 1.23 on 2026-06-07 when we
  adopted `folbricht/desync` v1.0.1 — desync's current release and modern grpc
  both require Go ≥1.25). The Makefile uses `GOTOOLCHAIN=auto` so the `go`
  command selects the matching toolchain (cached after first use). Pinning to
  1.23 with 2023-era deps was considered and rejected as less production-worthy.
- **buf/protoc are not assumed present in CI.** Generated code is committed
  (`proto/mirage/v1/*.pb.go`), so `go build`/`go test` work without buf. Only
  rerun `make proto` when you change the `.proto`.

## Code map

| Path | Role |
|---|---|
| `proto/mirage/v1/mirage.proto` | wire protocol; source of truth. Generated `*.pb.go` committed alongside. |
| `internal/chunk` | `Hash`, `Manifest`, desync-backed content-defined `Split` (`desync.NewChunker` + `desync.Digest` IDs), and the `Store` seam. |
| `client/index` | walks a dir, applies ignore + secret exclusion, chunks files → `(Manifest, chunkstore)`. |
| `client/chunkstore` | published chunks by hash; `Get` returns `found=false` for unpublished hashes. |
| `client/transport` | **the only Dialer.** Hello → IndexPublish → answer ChunkRequests. |
| `server/channelstore` | implements **`desync.Store`** over the stream: `GetChunk(id)` sends a `ChunkRequest`, awaits the matching `ChunkResponse` (correlated by `request_id`), returns a verified `desync.Chunk` (`NewChunkWithID`). No ctx in the signature (desync's interface) — the stream context + per-fetch timeout live inside the store. Safe for concurrent use. |
| `server/transport` store chain | reuses desync: `desync.NewCache(desync.NewDedupQueue(channelstore), LocalStore)` — local disk cache → single-flight → channel. No bespoke cache (see `docs/desync-reuse-review.md`). |
| `server/fuse` | thin tree FUSE presenting the manifest as a POSIX dir tree; file `Read` faults chunks lazily via `ReadRange` over the store chain. `ReadRange`/`IndexFromRefs` are pure + unit-tested; `Mount` needs a FUSE module at runtime. Live test (`TestLiveMount`) skips without FUSE; validate via `make fuse-validate` (Docker). |
| `internal/logging` | `log/slog` setup (level + text/json), nil-safe `OrDefault` injection for libraries. |
| `server/transport` | accepts the stream, sends `HelloAck`, and on `IndexPublish` *drives* one of two modes. **Reconstruct** (`New`, `--out`): write the tree to disk. **Mount** (`NewMounter`, `--mount`): FUSE-mount it so reads fault chunks lazily. The recv loop keeps dispatching `ChunkResponse`s while the driver runs; the driver owns cleanup (unmounts before the handler returns). Reports a `Result`. |
| `test/` | end-to-end over real localhost gRPC; asserts byte-identical trees and that secrets were never reconstructed. |

## Protocol flow (current milestone)

```
client.Dial -> Connect(stream)
client  -> Hello
server  -> HelloAck
client  -> IndexPublish{caidx = JSON manifest}
server  -> ChunkRequest{request_id, [hash]}   (one per needed chunk)
client  -> ChunkResponse{request_id, [Chunk]} | {error} if hash unpublished
...repeat until all chunks fetched...
server reconstructs files into --out, then RETURNS from Connect -> stream closes
client  sees io.EOF -> Serve returns
```

## Conventions

- Errors wrapped with `%w` and a package prefix (`channelstore: ...`).
- Tests live beside their package; the cross-package end-to-end test is in
  `test/`. Keep `go test ./...` green — it's the milestone gate.
- Paths in the manifest are slash-separated and relative to the workspace root;
  the server `safeJoin`s them and rejects traversal outside `--out`.

## Out of scope this round (don't build yet)

TUI, FUSE, TLS, auth, NAT traversal, multiple/concurrent connections,
write-back, prefetch, git fast-path, search index. Next up (design §9): a
server-side FUSE mount so a real POSIX read faults chunks via `ChunkRequest`.
