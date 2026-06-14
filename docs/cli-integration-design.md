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
*(Full design + task breakdown in §5 and §6 — this is the next build target.)*
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

---

## 5. The Go data-plane proxy (detailed design)

The proxy is the **authenticated, session-affine ingress** for the Mirage gRPC
stream. Path: `mirage-client → ALB → Go proxy (fleet) → mirage-server (task)`.
Its entire job is: authenticate the session, route to the right task, and
**transparently forward the long-lived bidirectional gRPC stream** — without
ever understanding Mirage's messages.

### 5.1 Responsibilities (and non-responsibilities)

**Does:**
- Terminate the inbound gRPC/HTTP-2 connection from the ALB.
- Read the **session token** once, at stream open (from gRPC metadata).
- **Authenticate** the token (gatekeeper; mirage-server re-validates independently).
- Resolve **session → task address** (via the session service).
- Dial the task's mirage-server and **splice the bidi stream** both ways until
  either side closes — forwarding raw message frames, never deserializing them.
- Pass **heartbeats through transparently** (they're end-to-end client↔server).
- Export health + metrics; shut down gracefully (drain live streams).

**Does NOT:**
- Parse, inspect, or buffer chunk payloads (it moves bytes).
- Hold session state beyond the live connection (it's **stateless**).
- Make materialization, write-back, or reconnect decisions (those are
  mirage-server's). On reconnect it simply re-resolves token→task again.
- Terminate or generate heartbeats.

### 5.2 Implementation approach (gRPC transparent proxy)

Mirage exposes one bidi-streaming method (`Mirage.Connect`). The proxy forwards
it without a generated stub using the standard Go transparent-proxy pattern:

- A `grpc.Server` configured with:
  - a **raw passthrough codec** (`grpc.ForceServerCodec`) whose Marshal/Unmarshal
    pass `[]byte` frames through untouched — so the proxy never deserializes
    Mirage messages; and
  - an **`grpc.UnknownServiceHandler`** that handles *any* method (the proxy
    registers no service) — this is the forwarder.
- The forwarder:
  1. pulls metadata from the incoming `ServerStream` context, extracts + validates
     the token, resolves the backend address;
  2. dials a `grpc.ClientConn` to the task (or reuses a pooled one) and opens a
     client stream to the **same full method** with the raw codec;
  3. runs **two goroutines** pumping frames concurrently — client→backend and
     backend→client — via `RecvMsg(&raw)` / `SendMsg(&raw)`, propagating
     headers, trailers, and final gRPC status;
  4. tears both sides down when either ends.

Reference: the `mwitkow/grpc-proxy` `StreamDirector`/`TransparentHandler`/`Codec`
pattern. It predates current grpc-go APIs (codec is now `encoding.Codec` with
`Name()`, and `ForceServerCodec` replaced the old registration), so **use the
pattern, adapt to current grpc-go** — ~300 LOC if reimplemented.

L7 (gRPC-aware) is chosen over L4 TCP splicing because we need to read the token
from gRPC metadata and propagate gRPC status/trailers cleanly. The cost (HTTP-2
framing) is negligible next to chunk throughput.

### 5.3 Token, auth, and routing

- **Where the token lives:** gRPC metadata header (e.g. `x-mirage-session` /
  `authorization`), read from the opening stream's context. (mirage-client sets
  it; it also flows in `Hello` so mirage-server can re-validate — §3 auth.)
- **Auth at the proxy:** verify the token offline (signed token) or via the
  session service; reject invalid/expired with `UNAUTHENTICATED` **before**
  dialing any backend.
- **Routing:** resolve token/session → **task address** (a Service Connect
  endpoint or IP:port). The session service owns the session→task-ARN map, so it
  must expose a **lookup that returns a routable address** for the session
  (ARN alone isn't dialable). Resolve **per connection** (don't cache across the
  connection's life — keeps reconnect correct and the proxy stateless); a short
  TTL cache is OK. Lookup miss / task gone → `UNAVAILABLE`/`NOT_FOUND`.

### 5.4 Long-lived stream handling

- **Idle timeouts:** disable or set very generous timeouts on the proxied stream
  (sessions last minutes–hours). Heartbeats (≤60s) keep it active; the proxy must
  not kill a live long stream. Keep the proxy's own keepalive enforcement lenient
  enough not to drop heartbeating clients (`grpc.KeepaliveEnforcementPolicy` /
  `ServerParameters` tuned to the heartbeat interval).
- **Bidirectional concurrency:** both directions flow independently (client→server:
  Hello/IndexPublish/ChunkResponse/heartbeat/write-back-result; server→client:
  HelloAck/ChunkRequest/write-back-batch/heartbeat). Pump them in separate
  goroutines; don't serialize.
- **Backpressure:** rely on gRPC/HTTP-2 flow control; don't add unbounded buffers.

### 5.5 Failure & teardown

- Token invalid/expired → `UNAUTHENTICATED`; routing miss → `UNAVAILABLE`;
  backend dial fail → `UNAVAILABLE` — all before/without corrupting a session.
- **Backend stream drops** mid-session → propagate the error/status to the client
  so it knows to reconnect.
- **Client drops** → close the backend stream; mirage-server then starts its
  grace timer (§3 reconnect). The proxy keeps no state.
- **Reconnect** is automatic: a fresh client stream re-resolves token→task to the
  **same task**, possibly via a **different proxy instance** (statelessness makes
  this fine).

### 5.6 Statelessness & fleet

The proxy holds nothing per-session beyond the live connection, so run it as a
**horizontally-scaled fleet behind the ALB**. Any instance handles any session;
a reconnect landing on a different instance still routes correctly. No SPOF,
no sticky sessions required.

### 5.7 TLS & network

- Inbound: ALB terminates client TLS; ALB→proxy per your existing scheme.
- Proxy→task: use the same model as today (Service Connect / mTLS, or plaintext
  within the VPC gated by **security groups so only the proxy can reach a task's
  mirage-server port**). The security-group lockdown is also what lets
  mirage-server's auth be defense-in-depth rather than the sole gate.

### 5.8 Config & observability

- **Config:** listen addr; session-service endpoint (token validation + task
  lookup); token verification key/secret; dial timeout; idle timeout (generous);
  TLS material; keepalive params.
- **Metrics:** active streams, bytes/sec per direction, dial latency, auth
  failures, routing failures/misses, stream lifetimes. **Health:** HTTP/gRPC
  endpoint for the ALB. **Logs:** structured, per-session (no payloads).

---

## 6. Go proxy — task breakdown

Ordered; each is independently testable. Effort: S (≤2d) · M (~1wk).

| # | Task | Eff | Done when |
|---|---|---|---|
| P1 | **Service skeleton** — gRPC server, config loading, health endpoint, structured logging, graceful shutdown scaffold. | S | Starts; health returns 200; accepts a gRPC connection; clean SIGTERM. |
| P2 | **Transparent bidi forwarder (core)** — raw passthrough codec + `UnknownServiceHandler`; forward to a **hardcoded** backend; two-goroutine pump; propagate headers/trailers/status. | M | `mirage-client → proxy → mirage-server` works end-to-end against a static backend: chunks flow, heartbeats pass through, a real workspace materializes. The make-or-break milestone. |
| P3 | **Token extraction + auth** — read the session token from metadata; verify (signed-token or session-service call); reject invalid/expired with `UNAUTHENTICATED` before dialing. | M | Bad/absent/expired token rejected; valid token proceeds. |
| P4 | **Session→task resolution** — session-service lookup returning a routable task address; per-connection resolve + short-TTL cache; miss → `UNAVAILABLE`. | M | Token resolves to the correct task address; lookup failure surfaces cleanly. |
| P5 | **Dynamic routing (wire P3+P4 into the director)** — the forwarder dials the resolved backend per connection. | S | Two concurrent sessions route to two different tasks correctly. |
| P6 | **Long-lived stream robustness** — generous/disabled idle timeouts; keepalive tuned to the heartbeat interval; half-close; error propagation both ways; clean teardown. | M | A multi-minute idle-but-heartbeating stream stays up; backend drop propagates to the client; client drop closes the backend. |
| P7 | **Reconnect routing** — confirm a fresh client stream re-resolves token→task to the **same** task, including via a **different proxy instance**. | S | Kill client mid-session, reconnect → routes to the same task (verified across two proxy instances). |
| P8 | **Observability** — metrics (active streams, bytes/dir, dial latency, auth/route failures), per-session structured logs. | S | Metrics exported and scrapeable; dashboards possible. |
| P9 | **TLS & network lockdown** — TLS on the hops per your model; security groups so only the proxy reaches task mirage-server ports. | M | Encrypted hops; direct task access from outside the proxy is blocked. |
| P10 | **Load & soak** — many concurrent long-lived streams, sustained chunk throughput + heartbeats; measure overhead vs. a direct connection; check for leaks. | M | Target concurrency sustained; no fd/goroutine/memory leaks over a multi-hour soak; overhead acceptable. |

**Suggested path:** P1 → **P2 (prove the splice first — biggest risk)** → P3/P4/P5
(auth + routing) → P6/P7 (robustness + reconnect) → P8/P9/P10 (ops + hardening).

**Dependencies on the rest of the system:** P3/P4 need the session service to
expose **(a) token validation** (or a verification key) and **(b) a session→task
*address* lookup**. P2 can proceed immediately against a local mirage-server.
The proxy does **not** depend on write-back or the reconnect server-side work —
it's a pure forwarder, so it can be built in parallel with those.
