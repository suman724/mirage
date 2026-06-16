# mirage-trace

A tiny Python package that lets an orchestrator opt a CLI session into Mirage's
**ptrace interception** with one call. It installs a small seccomp filter that
returns `SECCOMP_RET_TRACE` for the open + exec family; `mirage-server`
side-attaches via `ptrace` and materializes workspace files on demand as the
workload opens/executes them.

Background: `../docs/design-ptrace-interception.md` (§4.1) and
`../docs/how-ptrace-interception-works.md`.

## Usage

Call once, at the start of a CLI session, **before the workspace is touched**:

```python
import os, mirage_trace
mirage_trace.enable(os.environ["MIRAGE_ATTACH_SOCK"])
# ... then run the harness as normal; the filter is inherited by children.
```

`enable()`:

1. connects to `mirage-server`'s attach socket and sends `ATTACH <pid>`,
2. **blocks** until `mirage-server` replies `OK` (it has seized this process),
3. only then installs the `RET_TRACE` filter (open + exec family, `TSYNC`,
   `no_new_privs`).

The order is mandatory: a `RET_TRACE` syscall with no tracer attached returns
`-ENOSYS`. If the handshake fails, `enable()` raises `MirageTraceError` and
installs nothing (fail-closed). It is idempotent.

**Install it conditionally — only on the CLI path**, after the session is known
to be a CLI session. Never install it at a shared container entrypoint: web
sessions with no tracer attached would get `ENOSYS` on every open.

### CLI / non-Python callers

```sh
python -m mirage_trace "$MIRAGE_ATTACH_SOCK" -- your-workload --args
```

Attaches, installs the filter, then `exec`s the workload (which inherits the
filter) — the Python analogue of `shim/trace-launcher.c`.

## Requirements

Linux only. No required dependency: `enable()` uses the
[`libseccomp`](https://pypi.org/project/seccomp/) Python bindings if importable,
otherwise a `ctypes` raw-syscall fallback that builds the BPF by hand (x86_64 and
aarch64). Install `mirage-trace[libseccomp]` for the cleaner path.

## Coexistence

The filter has **no listener** (`SET_MODE_FILTER`, not `NEW_LISTENER`), so it does
**not** conflict with another component that owns the one seccomp notification
listener — that is the whole reason Mirage uses ptrace here rather than seccomp
user-notification.
