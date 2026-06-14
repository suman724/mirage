# Mirage ↔ CLI / autopilot-orchestrator Integration — Design Notes

**Status:** design captured from a brainstorm; not yet implemented. This records
the decisions and open items for integrating Mirage (FUSE-free seccomp mode)
into the existing autopilot stack to support a **CLI** client, reusing the same
backend the Web UI uses.

**Goal.** A thin CLI (TUI + embedded `mirage-client`) lets a user run the agent
harness in the backend against **their local files**. The harness runs in an
ECS task; Mirage projects the user's workspace lazily into that task and writes
the harness's edits back to the laptop.

---

## 1. Existing stack (context)

- **session service** (Python): session lifecycle; on new session → SQS
  message; DB table maps session → ECS task ARN; proxies Web UI traffic to the
  right task. Private subnet, behind an ALB.
- **autopilot-orchestrator** (Python, one process; the agent harness is part of
  it): a **pool** of ECS tasks scaled on SQS depth. Each task's poller waits for
  a message, picks one up, registers its task ARN with the session service, then
  runs the session. Tasks idle > 30 min are reaped.
- **Web clients** talk only to the session service, which proxies to the task.

---

## 2. Target architecture

```
CLI (laptop)              AWS (private subnet, behind ALB)
┌───────────────┐        ┌── session service (Python) ─ control plane ──┐
│ thin TUI      │ control│  lifecycle · session→task ARN · UI events     │
│ mirage-client │───────►│  issues/validates session tokens              │
└──────┬────────┘        └───────────────────────────────────────────────┘
       │ gRPC stream (chunks, heartbeats)
       ▼
   ┌── Go data-plane proxy (NEW) ──┐   session-token → task routing + auth
   │  validate token · route · splice bytes (dumb forwarder) · stateless fleet
   └──────────────┬────────────────┘
                  ▼
   ┌── autopilot-orchestrator ECS task ──────────────────────────┐
   │ mirage-server (PID 1, entrypoint)                            │
   │   └─ orchestrator (ONE python process: poller + harness)     │
   │        └─ git, rg, language servers, … (harness's tools)     │
   └──────────────────────────────────────────────────────────────┘
```

**Control plane = session service (Python).** **Data plane = Mirage file
transfer over a dedicated Go proxy.** Keep them separate — never route file
traffic through the Python service (it would bottleneck; it already proxies UI
events).

---

## 3. Decisions

### Process model
- **mirage-server is the container entrypoint (PID 1)** and launches the whole
  one-process orchestrator (poller + harness) as its workload. **No process
  split** — the orchestrator stays one Python process; it runs as mirage-server's
  child. mirage-server is the ancestor of the harness tree (required for
  seccomp's `/proc/<pid>/mem` reads under `ptrace_scope=1`) and PID 1 handles
  reaping + daemonized descendants.
- mirage-server must **launch its workload at startup** (not after the client
  publishes), because the poller must run immediately to grab the SQS message
  and register the ARN before any CLI connects. mirage-server builds the
  **skeleton on publish** and blocks any early `/workspace` open until ready.
  (This launch-at-startup / workload-outlives-connection model is also what
  reconnect needs — see §6.)

### Shared pool: web + CLI
- **One shared task pool.** mirage-server is always PID 1, even for web sessions
  (idle/unused there — harmless).
- **Open decision — the seccomp *filter*, not mirage-server, is the cost.** If
  the filter is always on, web sessions pay a per-open trap tax (every `open`
  round-trips to the supervisor for a no-op decision) and web workspaces **must**
  live outside the projected `/workspace`. Plan: **start with always-on (simple),
  guarantee web workspaces are outside `/workspace`, MEASURE the tax on a real
  web workload; if material, make the filter conditional on session type** (CLI
  only). Conditional install at the CLI moment needs either orchestrator
  self-install (Python — has the install→hand-off deadlock foot-gun) or launching
  the harness under the C launcher (a CLI-only subprocess).

### Connection, routing, auth
- **Custom Go data-plane proxy** (chosen over Envoy for stack fit + simple
  routing). Responsibilities: validate the session token, look up session→task,
  then **transparently splice the bidirectional stream** to the task's
  mirage-server. **Dumb byte-forwarder — never parse/buffer chunk payloads.**
  **Stateless fleet** (resolve token→task per connection) → no SPOF.
- **Routing is session-affine** (by token → task), which an ALB cannot do.
- **Auth is defense-in-depth:** the proxy validates (gatekeeper) **and**
  mirage-server validates independently (zero-trust task). Token must be
  **verifiable offline by both** — open choice: a signed token (session service
  issues; both verify) **or** session service injects the expected token to the
  task at pickup (via the poller's existing registration channel) and
  mirage-server compares. Avoid per-connection callbacks to the session service.

### Write-back (sandbox edits → laptop)
- **In scope for CLI** (the user must get the harness's edits back). Build it in
  **mirage-server + mirage-client first**, before the Go proxy.
- **On-demand, harness-triggered** at turn/task boundaries (matches how agent
  products work) — not continuous live-mirror.
- **Change set computed by a rescan** (walk `/workspace`, diff vs the manifest by
  size/mtime/hash) as the **source of truth**. This **defers namespace-syscall
  interception (#21)** entirely. Depends on **S4 (manifest mtime + size)** so the
  rescan is cheap (stat to skip unchanged; rehash only candidates).
- Apply on the laptop **confined to the workspace root**, with a **conflict
  check** (base-hash) + **confirm**. New content flows server→client as chunks
  (reverse of the read path). Existing proto messages (`WriteBackBatch`/
  `FileChange`/`WriteBackResult`/`FileApply`) already fit.
- Trigger wiring (how the harness says "sync now" to mirage-server) = a
  **direction**; harness-side completion detection is solvable.

### Disconnect / reconnect
- **Heartbeats end-to-end (client ↔ mirage-server), transparent through the
  proxy** — not terminated at the proxy. Server owns the hold/fail decision so it
  observes liveness directly; keeps the proxy dumb; validates the whole path;
  doubles as keepalive. Params: interval **30–60s**, **5** missed = disconnect
  (≈2.5–5 min detection); all **configurable**.
- **Bounded server-side grace, then fail.** On disconnect, hold the session
  (workload + skeleton + materialized state) ~**10–15 min** (tunable); reconnect
  within the window **resumes** (client re-dials a fresh stream, routed by token
  to the same task); on expiry, **kill the workload and reclaim the task**.
- The **grace timer is independent of the ALB idle timeout (~8 min)** — it holds
  the *session*, not the dead TCP connection. The old connection dies at the ALB;
  reconnect is a new stream. So the 8-min ALB idle does **not** cap the grace
  window. Heartbeat interval (≤60s) ≪ ALB idle (8 min) → keepalive is safe.
- Grace must **override the 30-min idle reaper**; an actively-faulting session
  must count as active.
- Accepted trade: failing on expiry discards edits **not yet synced** — bounded
  to the current turn (completed turns already synced).

### Build order
1. Mirage **write-back** (server + client) and **reconnect** (heartbeats + grace
   + re-attach), plus the launch-at-startup lifecycle change.
2. **S4** (manifest mtime + size) — prerequisite for the write-back rescan.
3. The **Go data-plane proxy** (routing + auth).
4. Auth validation in mirage-server; web/CLI filter measurement → conditional if needed.

---

## 4. Open items to design

- **Workspace-ready trigger** — *no trigger exists today.* The harness must learn
  the workspace is live (CLI connected + published) before it touches
  `/workspace`. Needs design.
- **Write-back trigger wiring** — the local channel from harness → mirage-server
  ("sync now"). Direction agreed; mechanism TBD.
- **Token verification mechanism** — signed token vs injected expected token.
- **Seccomp filter for web** — always-on vs conditional, gated on a measured tax.
- **Secret handling on write-back** — a secret created/edited in the sandbox must
  not silently round-trip to the laptop.
- **Size/storage envelope** — typical repo size + peak concurrency, to size the
  chunk cache bound (it's unbounded today) and Fargate ephemeral storage.
- **Scaling** (control-plane/session placement) — deliberately out of scope here;
  separate discussion.
