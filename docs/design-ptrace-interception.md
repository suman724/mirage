# Design — ptrace interception front-end

**Status:** design, not built. Engineering spec for Mirage's **ptrace-based**
interception front-end — the alternative to the seccomp user-notification
front-end. Plain-language background: `how-ptrace-interception-works.md`.
Tracking: GitHub milestone *"Ptrace interception front-end"* / `[Ptrace]` issues.

## 1. When this path is used

Use ptrace interception when the sandbox runs an OSS package that already owns
the **one** seccomp notification listener allowed per process tree (so Mirage
can't install its own), and/or to avoid making mirage-server the workload's
parent/PID 1. With `CAP_SYS_PTRACE` (allowed on Fargate) Mirage **side-attaches**
to the workload and intercepts its file syscalls — coexisting with the package's
seccomp listener (different mechanisms).

It is a **front-end swap only.** The core — skeleton builder, per-path state
table, `Materializer`, chunk store chain — is reused unchanged. Only "how an
`open`/exec is caught" differs from the seccomp loop.

## 2. Architecture

```
orchestrator (PID 1, unchanged)          mirage-server (side process, CAP_SYS_PTRACE)
   harness (+ tools)  ◄── PTRACE_SEIZE + follow-forks ──┤  tracer event loop
     │ open()/execve under /workspace                   │  reads regs+path, materializes,
     │ ── kernel stop (PTRACE_EVENT_SECCOMP) ──────────►│  then resumes the syscall
   (OSS package keeps its own seccomp NEW_LISTENER)
```

- mirage-server runs as a **separate process** (not the parent). It `PTRACE_SEIZE`s
  the harness and follows every descendant.
- The OSS package keeps its seccomp listener for its syscalls; Mirage uses ptrace
  for file syscalls. Coexist — different mechanisms, disjoint syscalls.

## 3. Chosen flavor: accelerated

**Decision: accelerated.** (Pure ptrace remains a documented fallback only if the
orchestrator filter can't be added.)

- **Accelerated (CHOSEN).** The **orchestrator installs** a small seccomp filter
  returning `SECCOMP_RET_TRACE` for the file-open + exec family (allow everything
  else); Mirage attaches with `PTRACE_O_TRACESECCOMP` and is stopped **only** on
  those syscalls → ~seccomp-level overhead. The filter is a non-listener filter
  (no `NEW_LISTENER`), so it stacks with the package's listener with **no
  `EBUSY`**, and they target disjoint syscalls. mirage stays a side process
  (orchestrator installs the filter, not mirage — see §4.1 for *why* and *how*).
- **Pure (fallback only).** No seccomp at all; Mirage uses `PTRACE_SYSCALL` and
  stops on **every** syscall, filtering itself. Zero orchestrator code, but heavy
  overhead (tens of % to ~2×). Use only if the orchestrator filter can't be added.

### Division of labor (read this — it's the common point of confusion)

- **mirage-server does ALL the interception:** attach, follow children, read the
  path on each open/exec stop, materialize, resume. It is a **separate process**
  (CAP_SYS_PTRACE).
- **The orchestrator does exactly ONE thing:** install the tiny `RET_TRACE`
  filter (the "notify mirage on open/exec" wiring). No path checks, no
  materialization, no interception logic. After installing it, the orchestrator
  runs the harness as normal and is unaware mirage exists.
- **Why the orchestrator and not mirage installs it:** a seccomp filter can only
  be installed by the process it applies to (on itself). mirage side-attaches —
  it is *not* the orchestrator's parent — so it physically cannot install the
  filter into the orchestrator. Hence the orchestrator self-installs; mirage does
  everything after.

## 4. The RET_TRACE filter (accelerated)

Installed by the orchestrator **when a CLI session begins** (NOT at the container
entrypoint — see §4.2): `prctl(PR_SET_NO_NEW_PRIVS, 1)` then a seccomp filter
(no listener, no fd — no deadlock risk; apply to all threads via TSYNC). Returns
`RET_TRACE` for:

- **Open family** (arch-correct — a missing entry is a silent hole → placeholder
  zeros): x86_64 `open`(2)/`openat`(257)/`openat2`(437)/`creat`(85);
  arm64 `openat`(56)/`openat2`(437) (no `open`/`creat`).
- **Exec family**: `execve`, `execveat` — see §6, this is mandatory.
- Everything else → `RET_ALLOW`.

Coexistence: the package's filter returns `USER_NOTIF` for *its* syscalls and
`ALLOW` for ours; our filter returns `TRACE` for ours and `ALLOW` for the
package's. Per-syscall the kernel evaluates both and the matching action wins
(disjoint sets → no contention). Install order doesn't matter.

### 4.1 Delivering the filter-install to the (Python) orchestrator

The orchestrator is Python, and it CAN install the filter — it's just
`prctl` + `seccomp` syscalls. Don't make the orchestrator team hand-roll BPF.
**Mirage ships the capability as a small package the orchestrator imports and
calls once:**

```python
import mirage_trace
mirage_trace.enable(workspace_root="/workspace", supervisor=…)  # at CLI-session start
# … then run the harness as normal
```

`mirage_trace.enable()` encapsulates everything fiddly:
1. tell mirage-server "attach to me (PID …)";
2. **wait until mirage confirms it has seized the process** (ordering — §4.2);
3. install the `RET_TRACE` filter (open+exec family, all threads via TSYNC,
   `no_new_privs`);
4. return.

Implement it with the **libseccomp Python bindings** (`seccomp` package —
arch-aware, supports the `TRACE` action, handles `no_new_privs`/TSYNC; cleanest)
or self-contained **ctypes** (no dependency; you maintain the per-arch syscall
numbers). Either way the orchestrator only ever sees `enable()`. (A `mirage-trace
enable` CLI can wrap the same code for non-Python callers.)

### 4.2 Install timing — conditional, and AFTER attach (avoids ENOSYS)

A `RET_TRACE` syscall with **no tracer attached returns `-ENOSYS`** (the call
fails). Two consequences:

- **Conditional, not at the entrypoint.** In the shared web+CLI task pool, a task
  only learns it's a CLI session after the SQS pickup. If the filter were
  installed at the container entrypoint, **web sessions (no mirage attached)
  would get `ENOSYS` on every open/exec and break.** So install it **only on the
  CLI path**, after pickup — which is exactly why it's a *callable* the
  orchestrator invokes, not a startup wrapper. (A startup wrapper would only be
  safe with split web/CLI task definitions.)
- **Attach before install.** mirage must be attached before the filter starts
  generating trace events, or the orchestrator's own opens hit `ENOSYS`. Hence
  `enable()` orders it as attach → confirm → install.

## 5. The tracer (mirage-server)

- Attach: `PTRACE_SEIZE` the workload with options
  `TRACEFORK|TRACEVFORK|TRACECLONE` (follow the whole tree),
  `TRACESECCOMP` (receive `PTRACE_EVENT_SECCOMP` stops), `EXITKILL` (kill the
  tracees if mirage dies — clean teardown).
- Event loop: `waitpid` for stops. On `PTRACE_EVENT_SECCOMP`:
  1. `PTRACE_GETREGS` → syscall nr + args (path pointer, dirfd, flags).
  2. Read the path string from the tracee's memory (`process_vm_readv` or
     `/proc/<pid>/mem` — allowed cross-process with `CAP_SYS_PTRACE`).
  3. Resolve relative paths: `dirfd` via `/proc/<pid>/fd/<dirfd>`, `AT_FDCWD`
     via `/proc/<pid>/cwd`.
  4. If under `/workspace` → `Materializer.Ensure(rel)` (reused). Else skip.
  5. `PTRACE_CONT` → the original syscall runs against now-real content
     (CONTINUE-after-materialize, same as the seccomp path).
- New-child stops (`PTRACE_EVENT_FORK/VFORK/CLONE`) → continue; the child is
  auto-traced and inherits the filter.

Response strategy is CONTINUE-after-materialize (simple, correct). Register
rewrite / fd injection is a possible future hardening, not required.

## 6. Coverage & correctness (read carefully)

- **Open family fully caught** at the syscall layer regardless of
  language/library/static — the strength over LD_PRELOAD — *provided the filter
  lists the complete arch-correct set* (§4).
- **`execve`/`execveat` are NOT opens.** Executing a file *from* the workspace
  (`./build.sh`, a repo-local binary, `node_modules/.bin` tools) reads its bytes
  **kernel-internally during exec**, bypassing any open filter. Mirage MUST trap
  exec and **materialize the target before allowing it**, or the exec runs from
  placeholder zeros and fails. (A dynamically-linked program's `.so` loads do go
  through `openat` = caught; it's the exec'd file's own bytes that are missed.)
- **`mmap` is fine** — it needs a prior `open` (caught) and we materialize the
  **whole file** at open, so page-faults hit real bytes. (Per-chunk would break
  mmap — whole-file materialize is mandatory.)
- **`open_by_handle_at`** opens by opaque handle (no path) — can't path-filter;
  rare (privileged) — ignored.
- **RET_TRACE ordering:** a `RET_TRACE` syscall with **no tracer attached**
  returns `-ENOSYS`. So mirage MUST be attached before the workload makes any
  trapped syscall. Sequence: orchestrator installs filter → mirage attaches →
  only then the workload touches `/workspace` (ties to the workspace-ready
  trigger).

## 7. Attach bootstrapping & lifecycle

- **PID discovery:** mirage needs the workload PID to seize it. Options: the
  orchestrator hands mirage its PID over a local channel, or mirage seizes its
  known target. Decide in implementation.
- **Ordering:** attach (with the filter already installed) before workspace
  access — else `ENOSYS` (§6).
- **Teardown:** `PTRACE_O_EXITKILL` so tracees die if mirage exits; on disconnect,
  mirage detaches/holds per the reconnect design.

## 8. Reused unchanged

`server/shim` (skeleton, state table + journal, `Materializer`) and
`server/channelstore` + the desync store chain. The ptrace front-end is the only
new component; it calls `Materializer.Ensure(rel)` exactly as the seccomp loop
does.

## 9. Overhead & flavor decision

Each ptrace stop ≈ a few–20 µs (two context switches + tracer work). Accelerated
stops only on the open/exec family (a small fraction of syscalls) → roughly the
same as the seccomp design. Pure stops on everything → heavy. **Decision pending
a prototype** that measures overhead on a *real* agent run (workspace read + a
build + tool spawns + executing a workspace script) from the actual harness. Use
accelerated; fall back to pure only if the orchestrator filter can't be added.

## 10. Validation plan

Docker harness (analogous to `make seccomp-server-validate`), UNPRIVILEGED except
`CAP_SYS_PTRACE`, asserting:
- a libc tool, a static Go binary, **and an *executed* workspace script/binary**
  all see real content (no placeholder zeros) — exec path explicitly covered;
- coexistence: a stand-in process holding its own seccomp `NEW_LISTENER` runs in
  the same tree without `EBUSY`, while Mirage's ptrace interception still works;
- laziness (untouched files stay sparse);
- a rough per-open / per-exec overhead number.

## 11. Open questions / risks

- ptrace × seccomp-notify interaction with the *actual* package in one process
  (validate empirically).
- Multi-threaded tracees and `waitpid`/stop handling at scale (correctness +
  throughput under many concurrent stops).
- Performance on real workloads (the gating measurement).
- Signal handling / `PTRACE_CONT` with pending signals; group-stop edge cases.
- Detach/re-attach semantics across a client disconnect (reconnect design).

## 12. Tasks

See the `[Ptrace]` issues under the *"Ptrace interception front-end"* milestone
(tracking issue links the set): RET_TRACE filter, tracer core, open handling,
exec handling, attach/ordering, CAP + coexistence validation, overhead
prototype, Docker harness, flavor decision.
