# Mirage on Fargate — Productionization & Scaling Handoff

**Purpose.** Mirage's FUSE-free lazy workspace ("Shimmer," via seccomp) is
**proven on real ECS Fargate**. This document is the engineering hand-off for
taking it from "validated prototype" to a **production, multi-tenant, scalable
service** — and for building the **write-back** half (sandbox edits → laptop)
that is designed but not yet implemented.

**Who this is for.** An engineering team picking up the codebase. It assumes you
will read, in this order: [`docs/how-mirage-works.md`](./how-mirage-works.md)
(the system from scratch), [`docs/how-shimmer-works.md`](./how-shimmer-works.md)
(the seccomp path), and [`docs/design-shimmer.md`](./design-shimmer.md) (the
build spec). This doc is the *work inventory + scaling architecture* on top of
those.

**Status legend:** ✅ done & validated · 🟡 partial · ⬜ not started.
**Priority:** P0 (blocks production) · P1 (needed to scale / for the headline
use case) · P2 (important, not blocking) · P3 (optimization).
**Effort:** S (≤2d) · M (~1wk) · L (2–4wk) · XL (>1mo / needs its own design).

---

## 1. Where we are today (what's actually done)

**The core data path and both lazy-workspace mechanisms work and are tested.**

- **Content-addressed transfer:** client chunks a workspace (content-defined
  chunking via `desync`), publishes a tiny manifest, and the server faults chunk
  *contents* lazily over **one client-dialed gRPC stream** (the server never
  dials in). Dedup + on-disk cache + single-flight all reused from desync.
  Secrets are excluded at index time on the client (security boundary).
- **Four server modes**, all validated: `--out` (reconstruct), `--mount` (FUSE
  lazy read), `--shim` (LD_PRELOAD fallback), and **`--seccomp` (the production
  Shimmer mode)**.
- **Seccomp interception (the headline):** `mirage-server --seccomp <dir> --
  <workload>` runs as the container entrypoint (PID 1), builds a sparse-placeholder
  skeleton on publish, launches the workload under a tiny C launcher that
  installs a seccomp user-notification filter, and materializes each file the
  workload `open()`s — covering **every** binary (libc, Go, static) at the
  syscall layer. Reuses the same chunk store.
- **Validated on real Fargate:** laptop client → **ALB (gRPC target group, TLS)**
  → **ECS Service Connect sidecar** (re-encrypted) → **mirage-server** (PID 1,
  plaintext gRPC intra-task) → launcher → workload; files transfer end-to-end.
  Locally reproduced UNPRIVILEGED via `make seccomp-server-validate`.
- **Health checks:** gRPC `grpc.health.v1` (SERVING) + optional HTTP
  `--health-addr /healthz`.

**The transfer is one-way today: laptop → sandbox.** Edits made in the sandbox
workspace do **not** flow back to the laptop. That is the single biggest
functional gap (§3).

### The deployment topology that was validated

```
  laptop                          AWS
 ┌────────┐   TLS    ┌─────┐  TLS   ┌──────────────── ECS Fargate task ────────────────┐
 │ mirage │ ───────► │ ALB │ ─────► │ Service Connect ─plaintext─► mirage-server (PID 1)│
 │ client │ ◄─ gRPC ─┤(gRPC│ ◄───── │ sidecar proxy       gRPC     │  builds skeleton    │
 └────────┘  stream  │ TG) │        │                              │  seccomp supervisor │
  has the files      └─────┘        │                              └──► launcher ──► WORKLOAD (agent)
                                    └───────────────────────────────────────────────────┘
                                                                         opens files → traps → materialize
```

This works for **one** session per task. Multi-tenant scaling (§6) is a
control-plane + routing problem on top of this.

---

## 2. Finish the seccomp interception (make the mechanism production-correct)

The mechanism is proven, but the current response strategy and lifecycle are the
"simplest correct" versions. These items harden it. (Tracked in **issue #3**.)

| # | Item | Pri | Eff | What & why | Where |
|---|---|---|---|---|---|
| 2.1 | **ADDFD response hardening** | P1 | M | Today the supervisor replies `CONTINUE` (kernel re-runs the tool's `open`). That has a TOCTOU window: a *malicious* workload could swap the path between our check and the kernel's re-read. Correct for trusted agents; unsafe for untrusted code. Fix: open the file in the supervisor and inject the fd via `SECCOMP_IOCTL_NOTIF_ADDFD` (atomic `ADDFD_FLAG_SEND`, available on Fargate's 6.1 kernel) — no re-execution, no race. Must match the tool's original open flags. | `server/seccomp/seccomp_linux.go` |
| 2.2 | **PID-1 subreaper (zombie reaping)** | P1 | S | When mirage-server runs as PID 1, orphaned descendants (tools the workload spawned and abandoned) reparent onto it and become zombies unless reaped. Add a `SIGCHLD`/`wait` loop (or use a known subreaper pattern). | `server/main.go` / new |
| 2.3 | **Per-open performance: measure & tune** | P1 | M | *Every* `open` in the sandbox traps and blocks while the supervisor decides — including the hundreds of `/usr/lib` opens at process start. The non-workspace fast-path must be cheap; the worker pool sizing matters. Measure real workloads (`go build`, `npm install`, `pytest`) and tune. This is the main unquantified risk of the whole approach. | `server/seccomp` |
| 2.4 | **Concurrency correctness under load** | P1 | M | Many processes trapping simultaneously; the single-receiver + worker-pool design needs a load test (hundreds of concurrent opens) under the race detector and on Fargate. | `server/seccomp` + new test |
| 2.5 | **Chunk-fetch failure resilience** | P1 | M | If a chunk fetch fails mid-materialization (network blip, client disconnect), the open currently fails with `EIO`. Decide retry/backoff vs. fail-fast; a transient blip shouldn't corrupt a session. Relates to reconnect (§5). | `server/shim/materializer.go`, `server/channelstore` |
| 2.6 | **Workload lifecycle model** | P1 | L | Today the workload is launched **per client connection** (`serveSeccomp` per `Connect`). For a long-lived agent that should outlive a reconnect, this is wrong — a dropped connection kills the agent. Decide: persistent workload + reconnectable client, vs. session==connection. Big design choice; affects §5 and §6. | `server/transport/transport.go` |
| 2.7 | **arm64 / Graviton validation on Fargate** | P2 | S | Proven on x86_64 Fargate; arm64 only proven locally. Re-run on Graviton Fargate (cheaper). Mechanism is arch-independent; low risk. | n/a |
| 2.8 | **mmap / dlopen edge cases** | P2 | S | Whole-file materialize-on-open should make `mmap` and dynamic loading correct (real file backs the mapping), but explicitly test git packfiles, `ripgrep`, and a `dlopen`-heavy program. | new test |

**Note on laziness:** seccomp is lazy at the **file** level (whole file on first
open), not the **chunk** level — because once we hand a real fd to an outside
process the kernel serves its reads and `mmap` page-faults, which we can't
intercept. Per-chunk laziness survives only for in-process Go readers (§4, the
billy adapter). This is intrinsic to syscall-level interception, not a bug.

---

## 3. Write-back: sandbox edits → laptop (the missing half)

**This is designed but not built.** The wire protocol already defines
`WriteBackBatch`, `FileChange`, `WriteBackResult`, `FileApply`,
`PermissionRequest`, `PermissionResult` (see `proto/mirage/v1/mirage.proto`) —
**but there are zero Go handlers for them.** It needs its own design doc before
code. (Tracked in **#16**, foundation in **#21**.)

| # | Item | Pri | Eff | What & why | Where |
|---|---|---|---|---|---|
| 3.1 | **Namespace-syscall interception** (#21) | P1 | M | `open()`-only interception sees content writes but **not** tree-shape changes: `rename`, `unlink`, `rmdir`, `mkdir`, `link`, `symlink`, `chmod/chown`, `utimensat`. So the supervisor's `local` set — the exact set write-back must ship — is only a **lower bound**. Add these syscalls to the seccomp filter (cheap now — just more BPF entries + handlers updating the state table). **OR** a sync-time rescan (walk the workspace, diff vs manifest by size/mtime/hash) as a backstop. Acceptance: atomic-save, `rm`, `mkdir`, `chmod +x` all reflected. | `shim/launcher.c` (BPF), `server/seccomp`, `server/shim` (state table) |
| 3.2 | **Write-back design doc** | P1 | M | Before code: conflict model (the `base_hash` field), permission/confirmation UX, secret round-trip prevention, reverse chunk transfer, partial-failure semantics. | new `docs/design-writeback.md` |
| 3.3 | **Reverse chunk transfer** | P1 | L | New/modified file content must travel server→client as chunks (`new_chunk_hashes` + `inline_chunks` in `FileChange`). The server chunks the changed file; the client either has the chunks (dedup) or the server inlines them. Mirror of the existing client→server path. | `server/transport`, `client/transport` |
| 3.4 | **Conflict detection** | P1 | M | `FileChange.base_hash` = the content hash at publish time. On apply, the client checks the laptop file still matches base_hash; if not, it's a conflict (user edited it meanwhile) → prompt, don't clobber. | `client/` apply path |
| 3.5 | **Client-side apply + permission prompt** | P1 | M | Apply changes to the laptop workspace, **confined to the workspace root** (reject traversal), with a user confirmation (`PermissionRequest`/`Result`). | `client/` |
| 3.6 | **Secret round-trip prevention** | P0-for-3 | S | A secret *created/edited* in the sandbox must not be written back to the laptop silently (the exclusion boundary is one-way today). Decide policy. | `client/index` policy + apply |

**Sequencing:** 3.1 (or the rescan) is the foundation — write-back can't ship a
correct change set without it. Then 3.2 design, then 3.3–3.6.

---

## 4. Security (do before any untrusted exposure)

| # | Item | Pri | Eff | What & why | Where |
|---|---|---|---|---|---|
| 4.1 | **Authentication** (#13) | **P0** | M | **There is none today.** `Hello.session_token` exists but is never validated — anyone who can reach the ALB can open a `Connect` stream, publish a workspace, and pull/serve chunks. Enforce identity at Hello/HelloAck: session tokens, mTLS identities, or per-session pairing codes. The token also becomes the routing key for scaling (§6). | `server/transport` Hello handler, `client/transport` |
| 4.2 | **Authorization / session pairing** | **P0** | M | Bind a specific laptop ↔ a specific sandbox session, so client A can't attach to client B's sandbox. Ties to the control plane (§6). | control plane + transport |
| 4.3 | **Workload is untrusted code — contain it** | P1 | M | If the sandbox runs arbitrary agent/user code, review: can it reach the launcher hand-off socket and forge requests? Can it escape `/workspace`? Can it read another tenant's data? (One task per tenant is the simplest isolation — §6.) ADDFD (2.1) is part of this. | review + `server/seccomp` |
| 4.4 | **TLS on the transport** (#12) | P2 | S | In the validated deploy, the ALB + Service Connect handle encryption-in-transit; mirage-server speaks plaintext gRPC intra-task. Native TLS is only needed for deployments **without** an encrypting proxy. Keep as opt-in. | `server/main.go`, `client/transport` |

---

## 5. Transport & connection robustness

| # | Item | Pri | Eff | What & why | Where |
|---|---|---|---|---|---|
| 5.1 | **Reconnect / resume** (part of #15) | P1 | L | A dropped gRPC stream ends the session today. For real use (laptop sleeps, network blips) the client must reconnect and resume without losing the workload (depends on 2.6). | transport both sides |
| 5.2 | **Graceful shutdown of in-flight sessions** | P2 | S | SIGTERM → `GracefulStop` exists, but verify in-flight seccomp sessions drain cleanly: stop servicing, kill the workload group, flush state. (Process-group kill is in; confirm under load.) | `server/transport`, `server/main.go` |
| 5.3 | **Backpressure / flow control** | P2 | M | Under a fault storm (e.g. `grep -r`), many ChunkRequests queue on one stream. Verify gRPC flow control + the worker pool don't OOM or starve. | `server/channelstore` |

---

## 6. Scaling the service (multi-tenant architecture) — the big one

Everything above makes **one** task correct. Running this as a **service for many
users**, each with their own sandbox, is a distinct architecture problem the
current code does **not** address. The current server is single-session
(fixed `--seccomp` dir + workload; one workspace per process).

**The central challenge: routing a client to *its* task.** An ALB target group
*load-balances* across healthy tasks — but a client must reach the **specific
task** running *its* workspace/session, not a random one. Load balancing is the
wrong primitive for session affinity. You need:

| # | Item | Pri | Eff | What & why |
|---|---|---|---|---|
| 6.1 | **Control plane / session manager** | P1 | XL | A service that: creates an ECS task per session, tracks session→task mapping, assigns the client, and tears the task down on session end. This is new infrastructure. |
| 6.2 | **Session-affine routing / relay** | P1 | XL | Route a client to its task by **session token** (4.1), not round-robin. Options: (a) a relay/gateway the client dials that proxies to the right task by token (see #14, originally framed as NAT traversal but it's the same broker); (b) per-task addressing via service discovery; (c) one listener per task (doesn't scale). The client-dials model means the *initial* connect must land on the right task. |
| 6.3 | **Task lifecycle & warm pools** | P2 | L | Cold task start + skeleton build is session-start latency. Pre-warmed task pools (#17) cut it. Define idle timeout, max session length, cleanup. |
| 6.4 | **Capacity & storage limits** | P1 | M | Fargate ephemeral storage is finite (20 GB default, up to 200 GB). Materialized files + the chunk cache consume it. A large workspace + `grep -r` (materializes everything) can exhaust it. Need cache eviction (§7) and per-task sizing. |
| 6.5 | **Multi-tenancy isolation** | P1 | M | Simplest model: **one task per tenant/session** (hard isolation, no shared memory, matches the PID-1/seccomp design). If you ever co-locate sessions in one task, the security review (4.3) gets much harder. Recommend one-task-per-session. |
| 6.6 | **Autoscaling & cost** | P2 | M | Scale tasks with active sessions; cost is per-task-second. Model it. |

**Recommended scaling model (opinion):** one ECS task per active session
(one workspace, one client, one agent), fronted by a **control plane** that
provisions tasks and a **session-token-addressed gateway** that routes each
client to its task. This keeps the validated single-session server unchanged and
puts all multi-tenancy in the new control/routing layer.

---

## 7. Operational / production readiness

| # | Item | Pri | Eff | What & why | Where |
|---|---|---|---|---|---|
| 7.1 | **Bounded chunk cache** | P1 | M | `desync.NewLocalStore(..., StoreOptions{})` is **unbounded** — it grows on disk for the session's lifetime. Add an LRU/size cap or periodic eviction; critical given Fargate storage limits (6.4). | `server/transport` |
| 7.2 | **State journal growth/compaction** | P2 | S | The append-only state journal grows per ENSURE/DIRTY. Bound or compact it for long sessions. | `server/shim/state.go` |
| 7.3 | **Observability** | P1 | M | Add metrics: chunk fetches, cache hit rate, materialization latency, trap rate, per-open overhead, errors, active sessions. Export (Prometheus/OTel) + dashboards + alerts. Structured logging (slog) already exists. | new |
| 7.4 | **Cold-start latency for big trees** | P2 | M | Skeleton build is O(files) syscalls; for a huge monorepo measure and optimize (parallelize, lazy subtrees). | `server/shim/skeleton.go` |
| 7.5 | **Resource limits & OOM safety** | P2 | M | Bound supervisor memory under fault storms; set task CPU/mem; handle disk-full gracefully (not silent corruption). | server + task def |

---

## 8. Performance for real agent workloads (laziness wins)

Seccomp is per-file lazy; a repo-wide `grep`/search materializes everything.
These reduce that cost and are high-value for agent use. (Independent of the
core; pick up after P0/P1.)

| # | Item | Pri | Eff | What & why |
|---|---|---|---|---|
| 8.1 | **Search index published with the manifest** (#19) | P2 | L | Client builds a trigram/ctags index and publishes it; server answers "grep the repo" / "find symbol" without faulting every chunk. **Likely the biggest agent-workload win.** |
| 8.2 | **S4: manifest mtime + `--include-git`** (#4, #5) | P2 | M | Add `mtime` to the manifest + skeleton; opt-in `.git` indexing (config scrub, hooks excluded). Prerequisite so `git status` doesn't hash (and fault) the whole tree. |
| 8.3 | **S5: billy adapter for in-process go-git** (#6) | P2 | L | A `billy.Filesystem` over the chunk store gives the agent **per-chunk-lazy** git in-process (recovering chunk-level laziness for git, which seccomp loses for external tools). |
| 8.4 | **Prefetch heuristics** (#17) | P3 | M | Readahead within a file, sibling warmup, background manifest-order fill. Must respect the hash-based protocol. |
| 8.5 | **Git fast-path** (#18) | P3 | L | Answer `git status`/`diff` on the **client** (where the real `.git` lives), shipping results not data. |

---

## 9. Suggested sequencing

1. **P0 security before any shared/untrusted use:** 4.1 auth, 4.2 session pairing
   (these also unlock §6 routing).
2. **Harden the mechanism:** 2.2 subreaper, 2.1 ADDFD, 2.3 perf measurement,
   2.5 fetch resilience, 2.6 workload lifecycle decision.
3. **Write-back** (the missing half, high product value): 3.1 namespace syscalls,
   3.2 design, then 3.3–3.6.
4. **Scaling architecture in parallel:** 6.1 control plane + 6.2 routing,
   6.4/7.1 storage+cache bounding, 7.3 observability.
5. **Robustness:** 5.1 reconnect (with 2.6), 5.2/5.3.
6. **Laziness wins:** 8.2 → 8.1 → 8.3, then 8.4/8.5.

---

## 10. Pointers (code, build, validate)

**Code map** (full version in `CLAUDE.md`):
- `proto/mirage/v1/mirage.proto` — wire protocol (incl. the unimplemented
  write-back/permission messages). `make proto` regenerates.
- `internal/chunk` — chunking, manifest, the `Store` seam.
- `client/index`, `client/chunkstore`, `client/transport` — the laptop side
  (the only dialer; secret exclusion).
- `server/transport` — accepts the stream, drives the four modes
  (`serveSeccomp` is the production one).
- `server/seccomp` — the seccomp notification loop (Linux).
- `shim/launcher.c` — the C seccomp launcher. `shim/mirageshim.c` — the
  LD_PRELOAD fallback.
- `server/shim` — skeleton, state table+journal, the shared `Materializer`.
- `server/channelstore` — the custom store that fetches over the gRPC stream.
- `server/main.go` — entrypoint, flags, health endpoints.

**Build & validate:**
```bash
make build                    # binaries
make test / make test-race    # unit + integration (reconstruct path)
make seccomp-server-validate  # the production path: server --seccomp <- client over gRPC + health (Docker, UNPRIVILEGED)
make seccomp-validate         # the seccomp mechanism harness
make shim-validate            # LD_PRELOAD fallback
make fuse-validate            # FUSE mode (needs /dev/fuse + SYS_ADMIN)
```

**Run the production mode locally** (sketch):
```bash
mirage-server --addr :7777 --seccomp /workspace --seccomp-state /state \
  --seccomp-launcher /usr/local/bin/mirage-launcher --health-addr :8080 \
  -- <your-agent-command>
# then, from the laptop:
mirage-client --addr <server-or-ALB-host> --dir /path/to/project
```
Deploy `mirage-server` as the **container entrypoint (PID 1)**.

**Key invariants to preserve (don't break these):**
- The connection is **always** client→server. Only `client/transport` dials.
- The protocol is **chunk-hash based, not path based** — the server can only
  request hashes the client published; the client rejects others. This is the
  security boundary.
- Secret exclusion happens at **index-build time on the client**.
- The seccomp supervisor must be an **ancestor of the workload**
  (`ptrace_scope=1`) — run mirage-server as PID 1.

**Background reading:** `docs/how-mirage-works.md`,
`docs/how-shimmer-works.md`, `docs/design-shimmer.md`,
`docs/mirage-on-fargate.md` (why FUSE is out, options compared),
`docs/workspace-fs-and-transport.md` (original full design), `HANDOFF.md`.

**Issue tracker:** Shimmer #1–#10 (#3 = remaining seccomp items; #4/#5/#6 =
S4/S5), #21 = namespace syscalls (write-back foundation); Horizon #11–#20
(#13 auth, #15 connections, #16 write-back, #17 prefetch, #18 git fast-path,
#19 search index). Tracking issues #10 and #20.
