# Mirage on Fargate — Productionization Handoff

**Purpose.** Mirage's FUSE-free lazy workspace (via Linux **seccomp**) is proven
on real ECS Fargate. This document is the engineering hand-off to take it from
*validated prototype* to *production*. It is self-contained: everything you need
to understand the work is here.

**Deployment model (fixed assumption for this whole document):** **one session =
one ECS task = one client connection.** A task runs one `mirage-server`, projects
one workspace, serves one laptop client, and runs one workload (the agent). There
is no multi-tenancy inside a task. (How sessions are provisioned/placed across
tasks is a separate concern, out of scope here.)

Status: ✅ done · 🟡 partial · ⬜ not started.
Priority: P0 (blocks production) · P1 (core/correctness) · P2 (important) ·
P3 (optimization). Effort: S (≤2d) · M (~1wk) · L (2–4wk) · XL (needs its own design).

---

## 1. How it works today (enough to act on)

The laptop runs **mirage-client**; the ECS task runs **mirage-server**. The
client splits the workspace into content-hashed **chunks**, sends a tiny
**manifest** (file → chunk-hash list), and the server fetches chunk *contents*
lazily over **one gRPC stream the client dialed** (the server never dials back;
it sends requests *down* the open stream). Secrets are excluded on the client at
index time — the server can only ever request hashes the client published.

In the production **seccomp** mode, on connect the server:
1. builds a **skeleton** — a real directory of sparse placeholder files (true
   size/mode, zero content) so `ls`/`find`/`stat` work natively;
2. launches the **workload** under a tiny C **launcher** that installs a seccomp
   user-notification filter trapping `open()`;
3. acts as the **supervisor**: when the workload opens a workspace file, the
   kernel pauses it and notifies the server, which materializes that file (fetches
   its chunks from the client into the placeholder) and lets the open proceed.

This catches **every** binary — libc, Go, static — because it intercepts at the
syscall layer. It is lazy at the **file** level (whole file on first open; we
can't intercept a real fd's reads or `mmap` page-faults).

```
  laptop                    AWS                          ECS Fargate task
 ┌────────┐  gRPC stream  ┌─────┐                ┌──── mirage-server (PID 1) ────┐
 │ mirage │ ────────────► │ ALB │ ──► (sidecar)► │  gRPC + seccomp supervisor +  │
 │ client │ ◄── chunks ── │     │ ◄──────────────│  skeleton builder             │
 └────────┘   (server     └─────┘                │        │ launches              │
  has files    requests                          │        ▼                      │
               down the                          │  launcher ─► WORKLOAD (agent) │
               stream)                           │            opens file → trap ─┘
```

**mirage-server must run as the container entrypoint (PID 1)** so it is an
*ancestor* of the workload — required to read the trapped process's memory
(`ptrace_scope=1`) to learn which path it opened.

**Today the transfer is one-way (laptop → sandbox).** Sandbox edits do not flow
back to the laptop (§4). And a dropped connection destroys the session (§3) —
the two biggest gaps.

---

## 2. Finish the seccomp mechanism

The mechanism works; these make it production-correct.

| # | Item | Pri | Eff | What & why |
|---|---|---|---|---|
| 2.1 | **ADDFD response hardening** | P1 | M | The supervisor currently replies `CONTINUE` (kernel re-runs the workload's `open`). That leaves a time-of-check/time-of-use window: a *malicious* workload could swap the path between our check and the kernel's re-read. Fine for trusted agents, unsafe for untrusted code. Fix: open the file in the supervisor and inject the fd via `SECCOMP_IOCTL_NOTIF_ADDFD` (atomic; supported on the Fargate kernel) — no re-execution, no race. Must replicate the workload's open flags. |
| 2.2 | **PID-1 subreaper** | P1 | S | As PID 1, the server must reap orphaned descendants (tools the workload spawned and abandoned) or they become zombies. Add a `SIGCHLD`/`wait` reaping loop. |
| 2.3 | **Per-open performance: measure & tune** | P1 | M | *Every* `open` in the sandbox traps and blocks while the supervisor decides — including the hundreds of `/usr/lib` opens at process start. The non-workspace fast-path must be cheap; tune the worker-pool size. Measure real workloads (`go build`, `npm install`, `pytest`). This is the main unquantified risk of the approach. |
| 2.4 | **Concurrency under load** | P1 | M | Many processes trapping at once. The single-receiver + worker-pool design needs a load test (hundreds of concurrent opens) under the race detector and on Fargate. |
| 2.5 | **arm64 / Graviton on Fargate** | P2 | S | Proven on x86_64 Fargate; arm64 proven only locally. Re-run on Graviton (cheaper). Mechanism is arch-independent; low risk. |

---

## 3. Disconnect & reconnect (HIGH PRIORITY)

**Current behavior (verified in code) — a disconnect destroys the session:**

- The gRPC stream is the session's only lifeline. If it drops (laptop sleeps,
  network blip, client crash), the stream's context is cancelled.
- The server reacts by **SIGKILLing the workload's whole process group, stopping
  the supervisor, and returning.** The running agent and all its in-progress work
  are lost.
- Any chunk fetch in flight at the moment of disconnect fails; if the workload
  was mid-`open`, it receives an **EIO** error.
- There is **no resume.** A new connection is a brand-new session: it rebuilds
  the skeleton and relaunches the workload from scratch. (If a persistent
  `--seccomp-state` dir is configured, the *file* materialization state in the
  journal survives, so already-materialized files aren't re-fetched — but the
  workload process itself is gone.)

For a laptop on real networks this is unacceptable: every sleep/blip kills the
agent. Reconnect support is the work below.

| # | Item | Pri | Eff | What & why |
|---|---|---|---|---|
| 3.1 | **Decouple workload lifetime from the connection** | P0 | L | A client disconnect must **not** kill the workload. The supervisor + workload keep running; only chunk *service* pauses. This is the foundational change for everything else here. |
| 3.2 | **Pause, don't fail, on disconnect** | P1 | M | While the client is gone, a workspace open needing an un-cached chunk should **block** (hold the seccomp notification — the kernel keeps the process parked) up to a reconnect deadline, instead of returning EIO. Cached/already-materialized reads keep working offline. Today the per-fetch timeout (30s) would fire and fail — it must be suspendable across a disconnect window. |
| 3.3 | **Re-attach on reconnect** | P0 | L | On reconnect the client re-dials; the server binds the **new** stream to the **existing** supervisor/workload and resumes serving the blocked/queued faults — rather than starting a fresh session. Needs a stable **session identity** (a token the client presents — ties to auth, §5) so the reconnecting client re-attaches to *its* session. |
| 3.4 | **Resume, don't re-publish from scratch** | P1 | M | The manifest is session-frozen and the state journal already records what's materialized. Reconnect should reuse both (no skeleton rebuild, no re-fetch of materialized files). |
| 3.5 | **Reconnect deadline + cleanup** | P1 | S | If the client doesn't return within a timeout, tear the session down (kill the workload group, free the cache/skeleton). Make the timeout configurable. |
| 3.6 | **Enforce a single active connection per task** | P1 | M | **Bug today:** nothing stops a second concurrent client from opening a second `Connect`, which would spawn a second launcher + skeleton over the *same* directory → corruption. The server must allow only one active session: a reconnect *replaces* the prior stream; a genuinely concurrent second client is rejected. |

---

## 4. Write-back: sandbox edits → laptop (the missing half)

Designed but **not built**: the wire protocol defines the messages
(`WriteBackBatch`, `FileChange`, `WriteBackResult`, `FileApply`,
`PermissionRequest`, `PermissionResult`) but there are **zero handlers**. Needs
its own design doc before code.

| # | Item | Pri | Eff | What & why |
|---|---|---|---|---|
| 4.1 | **Track tree-shape changes (namespace syscalls)** | P1 | M | `open()`-only interception sees content writes but **not** `rename`/`unlink`/`rmdir`/`mkdir`/`link`/`symlink`/`chmod`/`chown`. So the supervisor's set of "changed files" is only a *lower bound* — exactly the set write-back must ship, so it has to be complete. Add these syscalls to the seccomp filter (cheap — more BPF entries + handlers), **or** do a sync-time rescan (walk the workspace, diff vs the manifest by size/mtime/hash) as a backstop. Acceptance: atomic-save, `rm`, `mkdir`, `chmod +x` are all reflected. |
| 4.2 | **Write-back design doc** | P1 | M | Decide: conflict model, permission UX, secret round-trip policy, reverse chunk transfer, partial-failure semantics. |
| 4.3 | **Reverse chunk transfer** | P1 | L | New/changed content travels server→client as chunks (mirror of the existing client→server path; the protocol already has the fields). |
| 4.4 | **Conflict detection** | P1 | M | Each change carries the content hash it was based on; on apply the client checks the laptop file still matches, else flags a conflict (don't clobber). |
| 4.5 | **Client-side apply + permission prompt** | P1 | M | Apply to the laptop, confined to the workspace root (reject traversal), with user confirmation. |
| 4.6 | **Secret round-trip prevention** | P1 | S | A secret created/edited in the sandbox must not be written back to the laptop silently. |

Sequence: 4.1 (foundation) → 4.2 design → 4.3–4.6.

---

## 5. Security

| # | Item | Pri | Eff | What & why |
|---|---|---|---|---|
| 5.1 | **Authentication** | **P0** | M | **There is none today.** The `Hello` handshake carries a session-token field that is never checked — anyone who can reach the server can open a stream, publish a workspace, and pull/serve chunks. Enforce identity at Hello (session tokens / mTLS / pairing codes). The same token is the session identity reconnect needs (§3.3). |
| 5.2 | **Contain the untrusted workload** | P1 | M | If the sandbox runs arbitrary code, review: can it reach the launcher's hand-off socket and forge requests? Escape `/workspace`? ADDFD (2.1) is part of this. (One session per task already gives hard process isolation.) |
| 5.3 | **TLS on the transport** | P2 | S | In the validated deployment, encryption-in-transit is handled in front of the server (ALB + sidecar); the server speaks plaintext gRPC inside the task. Native TLS is only needed for a deployment without an encrypting proxy. Keep opt-in. |

---

## 6. Operational readiness

| # | Item | Pri | Eff | What & why |
|---|---|---|---|---|
| 6.1 | **Bound the chunk cache** | P1 | M | The on-disk chunk cache is **unbounded** — it grows for the session's lifetime. With Fargate's finite ephemeral storage, a large workspace + a repo-wide read (which materializes everything) can fill the disk. Add an LRU/size cap or eviction. |
| 6.2 | **Bound/compact the state journal** | P2 | S | The append-only state journal grows per materialization/dirty event. Cap or compact it for long sessions. |
| 6.3 | **Observability** | P1 | M | Metrics: chunk fetches, cache hit rate, materialization latency, trap rate, per-open overhead, errors, session state. Export + dashboards + alerts. (Structured logging already exists.) |
| 6.4 | **Graceful shutdown** | P2 | S | SIGTERM already triggers graceful gRPC stop; verify an in-flight session drains cleanly (stop servicing, kill the workload group, flush state). |
| 6.5 | **Cold-start latency for big trees** | P2 | M | Skeleton build is O(files) syscalls; measure/optimize for a large monorepo. |
| 6.6 | **Disk-full / OOM safety** | P2 | M | Handle disk-full and memory pressure gracefully (clear error, not silent corruption). |

---

## 7. Laziness wins (for real agent workloads)

seccomp is per-file lazy, so a repo-wide search materializes everything. These
cut that cost. Independent of the above; pick up after P0/P1.

| # | Item | Pri | Eff | What & why |
|---|---|---|---|---|
| 7.1 | **Search index published with the manifest** | P2 | L | Client builds a trigram/symbol index and publishes it; server answers "grep the repo" / "find symbol" without faulting every chunk. Likely the biggest agent-workload win. |
| 7.2 | **Manifest mtime + opt-in `.git` indexing** | P2 | M | Add `mtime` to the manifest + skeleton, and let the client publish a scrubbed `.git`. Makes `git status` a metadata-only walk (no content faulted). Prerequisite for the git fast-path. |
| 7.3 | **Git fast-path** | P3 | L | Answer `git status`/`diff` on the **client** (where the real `.git` lives), shipping results not data — avoids server-side content reads entirely. (Note: the former "in-process go-git/billy adapter" idea is dropped — seccomp runs the real `git` binary transparently; lazy git comes from 7.2 + this.) |
| 7.4 | **Prefetch heuristics** | P3 | M | Readahead within a file, sibling warmup, background fill. Must respect the hash-based protocol (only published hashes). |

---

## 8. Suggested sequencing

1. **P0 before any untrusted/shared use:** 5.1 auth (also the session identity for reconnect).
2. **Survive real networks:** §3 disconnect/reconnect — 3.1 + 3.3 (decouple + re-attach), then 3.2/3.4/3.5/3.6.
3. **Harden the mechanism:** 2.2 subreaper, 2.1 ADDFD, 2.3 perf, 2.4 concurrency.
4. **Write-back** (the missing half): 4.1 → 4.2 → 4.3–4.6.
5. **Operational:** 6.1 cache bound, 6.3 observability, the rest of §6.
6. **Laziness wins:** 7.2 → 7.1 → 7.3/7.4.

---

## 9. The codebase (self-contained map)

| Area | Path | Role |
|---|---|---|
| Wire protocol | `proto/mirage/v1/mirage.proto` | Source of truth (incl. the unimplemented write-back/permission messages). `make proto` regenerates the Go. |
| Chunking | `internal/chunk` | Content-defined chunking, manifest, the `Store` seam. |
| Client | `client/index`, `client/chunkstore`, `client/transport` | Walks the dir, excludes secrets, the **only** dialer; serves chunk requests by hash. |
| Server transport | `server/transport` | Accepts the stream; drives the modes. `serveSeccomp` is the production one; `Connect` runs the recv loop. |
| Seccomp supervisor | `server/seccomp` (Linux) | The notification loop: receive trap → read path from `/proc/<pid>/mem` → materialize → respond. |
| Launcher | `shim/launcher.c` | C: installs the seccomp filter, hands the listener fd to the server, execs the workload. |
| Skeleton + state | `server/shim` | Sparse-placeholder builder, per-path state table + journal, the shared `Materializer`. |
| Channel store | `server/channelstore` | The `Store` that fetches a chunk by sending a request down the gRPC stream; 30s per-fetch timeout, cancels on disconnect. |
| Entrypoint | `server/main.go` | Flags, mode selection, gRPC + HTTP health endpoints. |

**Build & validate** (the seccomp validations run UNPRIVILEGED in Docker — the
Fargate property — and need no devices/capabilities):
```bash
make build                    # binaries
make test                     # unit + integration
make seccomp-server-validate  # production path: server --seccomp <- client over gRPC + health
make seccomp-validate         # the seccomp mechanism on its own
```

**Run the production mode** (deploy mirage-server as the container entrypoint / PID 1):
```bash
mirage-server --addr :7777 --seccomp /workspace --seccomp-state /state \
  --seccomp-launcher /usr/local/bin/mirage-launcher --health-addr :8080 \
  -- <agent-command>
# laptop:
mirage-client --addr <server-host> --dir /path/to/project
```

**Invariants to preserve (do not break):**
- The connection is **always** client→server; only the client dials.
- The protocol is **chunk-hash based, not path based** — the server can only
  request hashes the client published; the client rejects others. This is the
  security boundary.
- Secret exclusion happens at index time **on the client**.
- The seccomp supervisor must be an **ancestor of the workload** — run
  mirage-server as **PID 1**.
- The workspace is lazy at the **file** level under seccomp; don't assume
  per-chunk laziness for external tools.
