# Design — ptrace interception front-end

**Status:** front-end **built and validated** (Docker/arm64) — core mechanism,
`--ptrace` side-attach server mode, gRPC production path, and the `mirage_trace`
Python helper; see §13. Remaining: amd64 *runtime* validation (needs a native
amd64 host), reconnect, coexistence test, overhead measurement. Engineering spec
for Mirage's
**ptrace-based** interception front-end — the alternative to the seccomp
user-notification front-end. Plain-language background:
`how-ptrace-interception-works.md`. Tracking: GitHub milestone *"Ptrace
interception front-end"* / `[Ptrace]` issues.

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
calls once,** at the start of a CLI session, before the workspace is touched:

```python
import mirage_trace
mirage_trace.enable(os.environ["MIRAGE_ATTACH_SOCK"])  # at CLI-session start
# … then run the harness as normal
```

`enable()` does three things, in this mandatory order (the order avoids the
ENOSYS trap of §4.2):

```python
def enable(supervisor_sock, timeout=10.0):
    _request_attach(supervisor_sock, timeout)  # 1+2: ask mirage to seize us; BLOCK until "OK"
    install_trace_filter()                      # 3: only now (tracer guaranteed present)
```

**Handshake** (`_request_attach`): connect to mirage-server's unix socket
(`MIRAGE_ATTACH_SOCK`), send `ATTACH <pid>\n`, block until `OK\n`. **If anything
fails, raise and do NOT install the filter** — installing it without a tracer
would make every open return ENOSYS. Idempotent (guard against a second call).

**Filter** (`install_trace_filter`) — libseccomp variant (cleanest):

```python
import seccomp
OPEN = ("open", "openat", "openat2", "creat")
EXEC = ("execve", "execveat")     # exec is NOT an open — must be trapped too (§6)
def install_trace_filter(msg=0):
    f = seccomp.SyscallFilter(defaction=seccomp.ALLOW)  # everything else runs normally
    f.set_attr(seccomp.Attr.CTL_TSYNC, 1)               # apply to ALL threads
    for name in OPEN + EXEC:
        try: f.add_rule(seccomp.TRACE(msg), name)        # SECCOMP_RET_TRACE
        except seccomp.SeccompSyscallResolveError: pass  # absent on this arch (e.g. open on arm64)
    f.load()   # sets no_new_privs + installs (SET_MODE_FILTER, no listener → coexists, no EBUSY)
```

**ctypes fallback** (no dependency): `prctl(PR_SET_NO_NEW_PRIVS,1)` then
`syscall(SYS_seccomp, SET_MODE_FILTER, FLAG_TSYNC, &prog)` with a hand-built BPF
returning `RET_TRACE` for the open/exec syscall numbers, `RET_ALLOW` otherwise —
same BPF shape as `shim/launcher.c`, swapping `RET_USER_NOTIF`→`RET_TRACE`,
adding the exec syscalls and the TSYNC flag. More code + per-arch numbers; prefer
libseccomp. (A `mirage-trace enable` CLI can wrap either for non-Python callers.)

**Server side of the handshake** lives in mirage-server (§5/§7): on
`ATTACH <pid>` it `PTRACE_SEIZE`s every thread in `/proc/<pid>/task` with the
options below, then replies `OK` — only after the seize succeeds.

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

- Attach (driven by the handshake, §4.1/§7): on `ATTACH <pid>`, `PTRACE_SEIZE`
  **every thread** in `/proc/<pid>/task` (re-scan to catch threads created during
  the loop) with options `TRACEFORK|TRACEVFORK|TRACECLONE` (follow the whole
  tree), `TRACESECCOMP` (receive `PTRACE_EVENT_SECCOMP` stops), `EXITKILL` (kill
  the tracees if mirage dies). Reply `OK` only after the seize succeeds.
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

- **Handshake protocol:** mirage-server listens on a unix socket
  (`MIRAGE_ATTACH_SOCK`). The orchestrator's `mirage_trace.enable()` connects and
  sends `ATTACH <pid>\n`; mirage seizes the process's threads (§5) and replies
  `OK\n`; only then does the orchestrator install the filter. This is the PID
  discovery + ordering mechanism in one.
- **Ordering:** seize must complete (mirage attached) **before** the filter is
  installed, and the filter before any workspace open — else `ENOSYS` (§6). The
  `enable()` sequence (attach → confirm → install) enforces this.
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

## 13. Implementation status (2026-06-15)

**Built and validated** (commit on `feature/shimmer`):

- `server/ptrace/` — the tracer. `Tracer.Serve(attachSock)` listens for one
  `ATTACH <pid>` request, `PTRACE_SEIZE`s the target + its threads (with
  `TRACEFORK|TRACEVFORK|TRACECLONE|TRACESECCOMP|TRACEEXEC|EXITKILL`), replies
  `OK`, then runs the wait/dispatch loop on a single locked OS thread. On each
  `PTRACE_EVENT_SECCOMP` it decodes the syscall + args via `PTRACE_GETREGSET`
  (arch files `regs_amd64.go` / `regs_arm64.go`), reads the path from
  `/proc/<pid>/mem`, resolves `*at` dirfd / `AT_FDCWD` via `/proc/<pid>/{fd,cwd}`,
  and calls `shim.Materializer.Ensure` before `PTRACE_CONT`
  (continue-after-materialize). Reuses the S1 materializer/skeleton/store
  unchanged. `//go:build !linux` stub keeps the module cross-building.
- `shim/trace-launcher.c` — `no_new_privs` → connect `MIRAGE_ATTACH_SOCK`
  (with connect-retry) → `ATTACH <pid>` → block for `OK` → **only then** install
  the `SECCOMP_RET_TRACE` filter (open **and** exec family, arch-correct, TSYNC)
  → `execvp` the workload. Ordering avoids the no-tracer `ENOSYS` (§4.2).
- `cmd/mirage-ptrace-harness` + `scripts/ptrace-validate.sh` + `make
  ptrace-validate`.

**Validation result (Docker, arm64, `--cap-add SYS_PTRACE`):** all checks pass,
`errors=0`. Confirmed (a) a **static Go binary** (raw `openat`, no libc) reads
real content via the open trap, and (b) **executing a workspace file** is
intercepted via the exec trap — the case that is *not* an open (§6). Laziness
holds (untouched files stay sparse). In the harness the tracer is the launcher's
real parent, so the seize works without `CAP_SYS_PTRACE`; the cap is added to
mirror production side-attach. **amd64 register/syscall decode compiles but is
not yet runtime-validated** (local Docker is arm64) — run on an amd64 host/CI.

**gRPC production path — validated.** `scripts/ptrace-server-validate.sh` /
`make ptrace-server-validate`: the REAL `mirage-server --ptrace` driven by the
REAL `mirage-client` over gRPC, with the workload attaching from an INDEPENDENT
process (`trace-launcher`) — the Fargate shape minus the ALB. A libc tool and a
static Go binary both read materialized files byte-identically, `/healthz`
answers 200, no placeholder zeros leak.

**`mirage_trace` Python package — built + validated.** `python/mirage_trace/`:
`enable(attach_sock)` does the attach handshake then installs the `RET_TRACE`
filter (open+exec, TSYNC, `no_new_privs`) — libseccomp if importable, else a
dependency-free ctypes raw-syscall fallback that hand-builds the BPF (x86_64 +
aarch64). Idempotent, fail-closed, Linux-only. Ships a `python -m mirage_trace
<sock> -- <cmd>` CLI for non-Python callers. `make mirage-trace-validate` proves
the orchestrator path end to end: Python self-installs the filter and the Go
tracer materializes a workspace file (`errors=0`, ctypes path).

**mirage-server `--ptrace` mode — built (side-attach).** Chosen over launcher
mode because side-attach is the reason ptrace exists (mirage-server is NOT the
workload's parent) and is structurally simpler (no child lifecycle, no
double-reap workaround). `transport.NewPtrace` + `servePtrace`: on IndexPublish
it builds the skeleton, then runs `Tracer.Serve(ctx, <state>/attach.sock)`. It
launches no workload — the orchestrator (or, in validation, an independent
trace-launcher process) connects and requests `ATTACH <pid>`, and the tracer
seizes that *non-descendant* via `CAP_SYS_PTRACE`. `Tracer.Serve` now takes a
context: on client disconnect it `PTRACE_INTERRUPT`s + `PTRACE_DETACH`es every
tracee (leaving the workload running) and returns `context.Canceled`. We also
dropped `PTRACE_O_EXITKILL` so a tracer crash auto-detaches rather than killing
the orchestrator's tree. **Fail-loud (G3):** a workspace file that fails to
materialize makes the open/exec fail with **EIO**, never a silent read of
placeholder zeros — implemented by neutralizing the syscall at its entry stop
(`orig_rax=-1` on amd64; `NT_ARM_SYSTEM_CALL=-1` on arm64) and overwriting the
return register with `-EIO` at the exit stop (`PTRACE_O_TRACESYSGOOD` two-stop).
Matches seccomp and the LD_PRELOAD shim. Validated by the `--fail-materialize`
fault-injection case in `ptrace-validate.sh`. Flags: `--ptrace DIR` / `--ptrace-state DIR` (mutually
exclusive with `--mount`/`--shim`/`--seccomp`). Validated by HEADLINE 3 in
`ptrace-validate.sh` (side-attach to a non-descendant, `errors=0`).

**Not yet built:**

- amd64 **runtime** validation. The arm64 dev host can't do it: QEMU emulation
  segfaults the Go/gcc toolchain and qemu-user can't faithfully emulate
  ptrace/seccomp. Resolution: run `make ptrace-validate` / `ptrace-server-validate`
  on a NATIVE amd64 host (the user's Fargate-class env or amd64 CI) — the scripts
  build and run for the host arch, so on amd64 they exercise `regs_amd64.go` and
  the x86_64 BPF directly. Code is ready; this is a CI/host task.
- Reconnect/resume after a disconnect-then-reattach (detach side is done; the
  re-attach + grace-window policy is CLI issue #33).
- Coexistence test with a real second seccomp `NEW_LISTENER` holder (§10).
- Overhead measurement on a real agent run (the gating §9 number).
