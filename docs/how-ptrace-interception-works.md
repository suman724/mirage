# How ptrace-based interception works (an alternative to seccomp)

**Audience:** someone who understands Mirage's lazy workspace (see
`how-shimmer-works.md`) and now needs the **ptrace** interception path — the
alternative to the seccomp front-end. Plain language; no debugger background
assumed.

If you only remember one sentence: **when something else in the sandbox already
owns the one seccomp notification listener, Mirage can instead use `ptrace` —
the debugger mechanism — to watch the workload's `open()` calls from the side
and materialize files just in time, and (with `CAP_SYS_PTRACE`) it no longer has
to be the workload's parent.**

---

## 1. Why a second mechanism exists

The seccomp path works by installing a *user-notification listener* that the
kernel calls when the workload opens a file. But the kernel allows **only one
such listener per process tree**. If an open-source package the agent uses
installs its **own** seccomp listener (for its own sandboxing/permission
purpose) and we can't change its code, Mirage can't also install one — the
second attempt fails with `EBUSY`.

So Mirage needs a *different* interception mechanism that coexists with the
package's seccomp listener. That mechanism is **ptrace**.

---

## 2. What Mirage's interception has to do (recap)

The workspace on disk is a **skeleton**: real directories and real file *names*
with correct sizes/modes, but empty (sparse) contents. The job of interception
is: catch the moment a program **opens** a workspace file, fetch that file's
content (as content-hashed chunks, over the connection the laptop dialed), write
it into the placeholder, and *then* let the open proceed — so the program reads
real bytes. Everything except "catch the open" is the same regardless of
mechanism; only the catch differs.

---

## 3. What ptrace is

**ptrace** ("process trace") is the kernel feature debuggers use: one process
(the **tracer**) can stop, inspect, and resume another (the **tracee**) — read
its registers and memory, and intercept its system calls. `strace` and `gdb`
are built on it. Mirage uses it the same way `proot` does: as the tracer, it
pauses the workload at each `open` syscall and services it.

---

## 4. How ptrace intercepts an open, step by step

```
 workload (tracee)            mirage (tracer)
      │  open("/workspace/foo")
      │ ─────────── kernel stops the workload at the syscall ──────────►│
      │                                          read registers (path ptr, flags)
      │                                          read the path string from tracee memory
      │                                          ── inside /workspace? ──► materialize foo
      │                                              (fetch chunks → fill placeholder)
      │ ◄──────────── tracer resumes the workload (PTRACE_CONT) ────────│
      │  open() now hits the real file → reads correct bytes
```

1. The workload calls `open`; the kernel **stops** it and notifies the tracer.
2. Mirage reads the workload's registers to get the path pointer and flags, then
   reads the path string out of the workload's memory.
3. If the path is under `/workspace`, Mirage materializes the file (faults its
   chunks into the placeholder) — the same materializer the seccomp path uses.
4. Mirage **resumes** the workload; the original `open` runs against the now-real
   file. (Same "fill, then continue" idea as the seccomp path.)

It attaches to the workload **and follows every child it spawns**
(`PTRACE_O_TRACEFORK`/`VFORK`/`CLONE`), so the agent and all its tools (git,
ripgrep, language servers) are covered — exactly how `proot` blankets a process
tree.

---

## 5. The big win: Mirage no longer has to be the parent

The seccomp path forced `mirage-server` to be the workload's **ancestor**
(ideally PID 1), because reading another process's memory under the default
`ptrace_scope=1` is only allowed for *descendants*. That drove a lot of
structural work (entrypoint inversion, launcher, lifecycle gymnastics).

**`CAP_SYS_PTRACE` removes that constraint** — a process holding it can trace and
read the memory of any same-user process *regardless of ancestry*. And
**Fargate allows `CAP_SYS_PTRACE`** (it's the one capability it permits). So:

- `mirage-server` runs as a **separate, side-attached process** — it
  `PTRACE_SEIZE`s the workload from the side; it does **not** need to be PID 1,
  the launcher, or the parent.
- The orchestrator stays the entrypoint and one process; no inversion, no split.

That simplification is the main reason to consider this path even apart from the
listener conflict.

---

## 6. Two flavors — and the overhead

The cost hinges on **how many syscalls you stop on.** Each ptrace stop is two
context switches plus the tracer's work — on the order of a few to ~20
microseconds. Overhead ≈ (stopped syscalls × per-stop cost) ÷ runtime.

- **Pure ptrace (stop on *every* syscall).** Simplest — no filter, nothing to
  install anywhere — but you pay the stop cost on *all* syscalls (you just
  resume the ones you don't care about). A Python agent plus its tools makes
  millions of syscalls, so this is the **heavy end: tens of percent to ~2×** for
  syscall-heavy phases.

- **Accelerated (stop only on the open family) — recommended.** Install a small
  seccomp filter that returns **`SECCOMP_RET_TRACE`** for the open family and
  *allow* for everything else; Mirage attaches with `PTRACE_O_TRACESECCOMP` and
  is stopped **only** on opens. Opens are a tiny fraction of all syscalls, so the
  interception overhead collapses to **roughly the same as the seccomp design —
  low single digits to ~25%.**

  Crucially, this **keeps the relaxed-parent property**: the tiny `RET_TRACE`
  filter is self-installed by the **orchestrator** (your code — just
  `no_new_privs` + the filter; no listener, no fd hand-off, none of the deadlock
  risk the seccomp listener had). Mirage still side-attaches. `RET_TRACE` is
  **not** a notification listener, so it does **not** collide with the package's
  listener.

One cost is the **same in every flavor** and isn't a ptrace cost: materializing
a file on its first open still fetches the whole file's chunks. That's inherent
to lazy projection.

---

## 7. Coexisting with the package's seccomp listener

ptrace and seccomp are **different kernel mechanisms** and run on the same
process at once. The open-source package keeps its seccomp notification
listener for its syscalls; Mirage uses ptrace for `open`. They don't fight:

- In the accelerated flavor there are two seccomp filters stacked — the
  package's (returns *notify* for its syscalls) and the orchestrator's (returns
  *trace* for opens). They target **disjoint** syscalls; the kernel evaluates
  both and the relevant action wins for each syscall. There's **no `EBUSY`**
  because only one of them is a notification listener.
- Mirage is stopped only on opens, so it never even sees the package's syscalls;
  the package's supervisor never sees opens.

---

## 8. What's reused vs. new

- **Reused unchanged:** the skeleton builder, the per-path state table, the
  materializer, and the chunk store chain. The decision "this path is under
  `/workspace`, fetch and fill it" is mechanism-agnostic.
- **New:** the front-end — `PTRACE_SEIZE` + follow-forks, handling syscall/seccomp
  stops, reading registers and tracee memory, and resuming. `proot` is the
  reference implementation. This replaces the seccomp notification loop; the
  rest of Mirage is untouched.

---

## 9. ptrace vs. seccomp — when to pick which

| | seccomp notification | ptrace (accelerated) |
|---|---|---|
| Coexists with another seccomp **listener** in the tree | ❌ (only one allowed) | ✅ (different mechanism) |
| Requires Mirage to be the workload's ancestor / PID 1 | ✅ (drives a lot of structure) | ❌ (side-attach with `CAP_SYS_PTRACE`) |
| Extra capability needed | none | `CAP_SYS_PTRACE` (allowed on Fargate) |
| Interception overhead | low (opens only) | ~the same (opens only) |
| Front-end complexity | built & validated | new (proot-style) |

**Pick ptrace when** something else owns the seccomp listener, or when avoiding
the PID-1/parent structure is worth a new front-end. **Pick seccomp when** Mirage
owns interception outright and you'd rather not add a capability or a new code
path.

---

## 10. Status & next

This is a **documented alternative**, not yet built. Before committing:

1. **Prototype and measure overhead on a real agent run** (workspace read + a
   build + tool spawns) from your own harness — the number is workload-shaped.
   Use the **accelerated** flavor; fall back to pure only if you can't add the
   tiny orchestrator filter.
2. Add `CAP_SYS_PTRACE` to the task definition.
3. Decide attach bootstrapping: Mirage needs the workload's PID to seize it
   (e.g., the orchestrator hands Mirage its PID, or Mirage attaches to its
   parent).
4. Validate the ptrace × seccomp-notify interaction with the actual package in
   one process.

If the measured overhead is acceptable, this becomes Mirage's interception
front-end whenever the sandbox already runs a seccomp-listener package.
