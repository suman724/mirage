# Mirage

A cloud Linux sandbox reads a user's workspace **as if the files were local** —
but the files are content-addressed chunks fetched lazily on read over **one
client-initiated connection**. The client (Win/Mac) dials out; the server
(sandbox) **never dials in**, yet it *requests chunks back down the already-open
stream* and the client serves them.

That inversion — client opens the socket, server originates requests on it — is
the core property Mirage proves.

> **New here?** Read [`docs/how-mirage-works.md`](./docs/how-mirage-works.md) — a
> plain-language tour of the whole system (no desync/chunking/FUSE background
> needed).
>
> **Start here for depth:** [`HANDOFF.md`](./HANDOFF.md) for orientation, then
> [`docs/workspace-fs-and-transport.md`](./docs/workspace-fs-and-transport.md)
> for the full design (esp. §2–§4, §6), and
> [`proto/mirage/v1/mirage.proto`](./proto/mirage/v1/mirage.proto) for the wire
> protocol. Task status lives in [`TASKS.md`](./TASKS.md).

## What works today (milestone: end-to-end data transfer)

Minimal end-to-end **data-transfer** validation over localhost — **no TUI, no
FUSE**. Proven:

1. the client dials the server and opens one gRPC bidi stream (`Mirage.Connect`);
2. the client publishes an **index** (manifest of chunk hashes) for a directory;
3. the **server** sends `ChunkRequest` down that same stream
   (server-originated requests on a client-opened connection);
4. the client serves chunk bytes by hash, and **rejects any hash not in the
   published index**;
5. the server reconstructs the files into an output dir, verifying each chunk by
   hash, and the integration test asserts the trees are byte-identical — with
   the server having read **only chunks off the stream**, never the source.

## Layout

```
proto/mirage/v1/   wire protocol (single source of truth) + generated Go
internal/chunk/    shared chunking primitives + the Store seam (GetChunk(hash)->bytes)
client/            DIALS OUT, indexes a dir, serves chunks by hash
  index/             walks the tree, excludes secrets (.env/*.pem/id_*), chunks files
  chunkstore/        holds published chunks; rejects unpublished hashes
  transport/         the ONLY package that Dials
server/            ACCEPTS the connection, drives the protocol, reconstructs files
  channelstore/      desync.Store whose GetChunk() = ChunkRequest over the stream
  transport/         accepts the stream; store chain = desync Cache->DedupQueue->channelstore
  fuse/              thin tree FUSE: POSIX reads fault chunks lazily over the chain
internal/logging/  structured log/slog setup shared by both binaries
test/              end-to-end integration test over real localhost gRPC
```

> **Chunker:** chunking and content hashing are backed by
> [`folbricht/desync`](https://github.com/folbricht/desync) — `internal/chunk`
> uses desync's content-defined chunker (16K/64K/256K) and desync chunk IDs
> (SHA-512/256), all behind the `chunk.Store` seam (`GetChunk(hash) -> bytes`).
> Identical content anywhere in the tree collapses to one chunk.

## Quickstart

```bash
make build          # -> bin/mirage-server, bin/mirage-client
make test           # unit + integration tests
make test-race      # the whole suite under the race detector
make proto          # regenerate Go from the .proto (needs buf; `make tools` once)
make fuse-validate         # live FUSE mount tests in a Linux container (needs Docker)
make shim-validate         # Shimmer LD_PRELOAD path + libc tool matrix, UNPRIVILEGED (needs Docker)
make seccomp-validate      # Shimmer seccomp interception (incl. a static Go binary), UNPRIVILEGED (needs Docker)
make seccomp-server-validate # the production path: mirage-server --seccomp driven by mirage-client over gRPC + health (needs Docker)
```

The server runs in one of **four** modes. **Reconstruct** (`--out`, the default)
writes the published tree to disk. **Mount** (`--mount <dir>`) FUSE-mounts the
workspace so a real POSIX read faults chunks lazily over the channel — the
"reads like a local FS" path. FUSE needs a kernel module (macFUSE on macOS,
`/dev/fuse` on Linux); `make fuse-validate` exercises it in a Linux container.

The two **Shimmer** modes give the same lazy workspace with **zero kernel
privileges** (for platforms that forbid FUSE, e.g. AWS Fargate). Both project a
real directory of sparse placeholders and materialize each file on first open:

- **Seccomp** (`--seccomp <dir> -- <workload>`, the production mode) runs the
  workload under a seccomp user-notification filter installed by a tiny C
  launcher; mirage-server is the supervisor and materializes files as the
  workload opens them, covering **every** binary — libc, Go, static — at the
  syscall layer. Run mirage-server as the container entrypoint (PID 1) so it is
  an ancestor of the workload (required to read its memory). `--seccomp-state`
  gives a restart-recoverable journal+cache; `--health-addr` adds an HTTP
  `/healthz` for load-balancer health checks (the gRPC health service is always
  registered). `make seccomp-server-validate` runs the full path over gRPC.
- **Shim** (`--shim <dir>`) is the LD_PRELOAD fallback for environments where a
  seccomp filter can't be installed: a C shim talks `ENSURE`/`DIRTY` to a
  supervisor socket. It is blind to Go/static binaries, which is why `--seccomp`
  superseded it. `make shim-validate` runs its libc tool matrix.

See [`docs/design-shimmer.md`](./docs/design-shimmer.md) §3.3 and the
plain-language [`docs/how-shimmer-works.md`](./docs/how-shimmer-works.md).

Manual localhost demo (two terminals):

```bash
# terminal 1 — server ACCEPTs and reconstructs into ./mirage-out
make run-server

# terminal 2 — client DIALs out and publishes the test tree
make run-client

# the reconstructed tree matches the source except the excluded secrets:
diff -r testdata/workspace ./mirage-out   # only .env and id_rsa differ
```

## Requirements

- Go 1.25 (the `go` command auto-selects the matching toolchain; desync v1.0.1
  and modern grpc require it)
- For `make proto`: [`buf`](https://buf.build) plus `protoc-gen-go` and
  `protoc-gen-go-grpc` (`make tools`)
