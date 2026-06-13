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
make build                   # bin/mirage-server, bin/mirage-client
make test                    # unit + integration (go test ./...)
make integration             # just the end-to-end test, verbose
make seccomp-server-validate # PRIMARY Shimmer: mirage-server --seccomp <- client over gRPC, Docker UNPRIVILEGED
make seccomp-validate        # seccomp mechanism harness (launcher+supervisor+matrix), Docker UNPRIVILEGED
make shim-validate           # LD_PRELOAD fallback + libc tool matrix, Docker UNPRIVILEGED
make fuse-validate           # FUSE mount tests in Docker (needs /dev/fuse + SYS_ADMIN)
make proto                   # regen Go from proto/ (buf); `make tools` installs plugins
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
| `internal/fsutil` | `SafeJoin`: traversal-rejecting join of untrusted manifest paths onto a server root (used by reconstruct + shim). |
| `server/shim` | **Shimmer** (docs/design-shimmer.md): FUSE-free lazy workspace for privilege-restricted platforms (Fargate). Skeleton builder (sparse placeholders, true size/mode), per-path state table (`placeholder/materializing/materialized/local`) with a synced JSON-lines journal (crash mid-fill replays as *torn* → re-fill), supervisor on a unix socket (`ENSURE/DIRTY/MATERIALIZE_ALL/STATS`, one request/conn), per-path singleflight, **pristine-placeholder check** before every fill (never clobber a replaced placeholder — §4.1). |
| `server/seccomp` | **the primary Shimmer interception** (Linux-only; design §3.3): the seccomp user-notification loop. Single receiver owns `NOTIF_RECV` (poll-gated on POLLIN — the listener ignores O_NONBLOCK), dispatches to a worker pool; resolves the path from `/proc/<pid>/mem` (dirfd/cwd-aware, ID_VALID-bracketed), calls `shim.Materializer.Ensure`, responds CONTINUE. `RecvListenerFd` receives the launcher's listener fd via SCM_RIGHTS. Covers Go/static binaries the LD_PRELOAD shim cannot. darwin stub keeps the module cross-building. |
| `shim/launcher.c` | the seccomp **launcher** (C, Linux-only): `no_new_privs` + a `NEW_LISTENER` filter trapping the open family (per-arch BPF), hands the listener fd to the supervisor via SCM_RIGHTS, then `execve`s the workload. Inherited by all descendants. The production interception entry. |
| `shim/mirageshim.c` | the LD_PRELOAD shim (C, Linux-only) — **fallback**, superseded by seccomp: interposes the open+fopen families, ENSUREs under `$MIRAGE_SHIM_ROOT`, fails loud (EIO). Validated via `make shim-validate`. |
| `server/transport` | accepts the stream, sends `HelloAck`, and on `IndexPublish` *drives* one of **four** modes. **Reconstruct** (`New`, `--out`). **Mount** (`NewMounter`, `--mount`): FUSE. **Shim** (`NewShimmer`, `--shim`): LD_PRELOAD socket supervisor (fallback). **Seccomp** (`NewSeccomp`, `--seccomp -- <workload>`, the production mode): builds the skeleton, spawns the C launcher + workload as its own child (so the server is the workload's ancestor — ptrace_scope=1), services open() traps via `server/seccomp` until the workload exits or the client disconnects. `shim.Materializer` is shared by the shim + seccomp paths. The recv loop keeps dispatching `ChunkResponse`s while the driver runs. Reports a `Result`. |
| `cmd/mirage-seccomp-harness` | Linux test harness: wires fixture→skeleton→materializer→supervisor→launcher→workload without gRPC (`make seccomp-validate`). |
| `server/main.go` | mirage-server entrypoint: flags for all modes, gRPC health service + optional HTTP `--health-addr /healthz` (ALB health checks). Run as PID 1 in seccomp deployments. |
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

TUI, TLS, auth, NAT traversal, multiple/concurrent connections, write-back
(deferred eventual goal, issue #16 — never a "non-goal"), prefetch, git
fast-path, search index ([Horizon] issues #11–#20). **Shimmer status:** S1
(skeleton+supervisor), S2 (LD_PRELOAD shim), the seccomp spike (#8, PASSED on
Fargate), and **S3′ (seccomp wired into `mirage-server --seccomp`)** are DONE
and validated — incl. on real Fargate behind an ALB. The exec gate is retired
(seccomp covers Go/static). Remaining Shimmer: ADDFD hardening, per-trap perf,
retire the C shim, then S4 (mtime + `--include-git`), S5 (billy/go-git).
Namespace-syscall interception reserved as #21. Tracking: issue #10.
