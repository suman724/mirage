"""mirage_trace — let an orchestrator opt a CLI session into Mirage's ptrace
interception with one call.

Mirage's ptrace front-end (docs/design-ptrace-interception.md) needs the
workload to run under a small seccomp filter that returns SECCOMP_RET_TRACE for
the open + exec family. mirage-server side-attaches (PTRACE_SEIZE) and, on each
trapped syscall, materializes the workspace file before the syscall proceeds.

The orchestrator installs that filter itself — it is just prctl + seccomp
syscalls — but should not hand-roll BPF. Import this and call once, at the start
of a CLI session, before the workspace is touched:

    import os, mirage_trace
    mirage_trace.enable(os.environ["MIRAGE_ATTACH_SOCK"])
    # ... then run the harness as normal; the filter is inherited by children.

enable() does three things in this MANDATORY order (see §4.2):
    1. connect to mirage-server's attach socket and send "ATTACH <pid>"
    2. BLOCK until mirage-server replies "OK" (it has seized us)
    3. only THEN install the RET_TRACE filter

The order matters: a RET_TRACE syscall with no tracer attached returns -ENOSYS,
so the filter must never be installed before the seize is confirmed. If the
handshake fails, enable() raises and installs nothing (fail-closed).

Install it CONDITIONALLY, only on the CLI path (after the session is known to be
a CLI session) — never at a shared container entrypoint, or web sessions with no
tracer attached would get ENOSYS on every open (§4.2).

No third-party dependency is required: enable() uses the libseccomp Python
bindings if present, else a ctypes raw-syscall fallback. Linux only.
"""

from __future__ import annotations

import os
import platform
import socket
import struct
import threading

__all__ = ["enable", "is_enabled", "MirageTraceError"]

# Syscalls trapped. exec is NOT an open — executing a workspace file bypasses an
# open-only filter, so it must be trapped too (design §6).
_OPEN_SYSCALLS = ("open", "openat", "openat2", "creat")
_EXEC_SYSCALLS = ("execve", "execveat")

_lock = threading.Lock()
_enabled = False


class MirageTraceError(RuntimeError):
    """Raised when attach or filter installation fails. On any failure the
    filter is NOT installed (fail-closed)."""


def is_enabled() -> bool:
    """True if enable() has already installed the filter in this process."""
    return _enabled


def enable(attach_sock: str, timeout: float = 10.0) -> None:
    """Attach mirage-server to this process and install the RET_TRACE filter.

    Idempotent: a second call is a no-op. Raises MirageTraceError if the attach
    handshake fails (and installs nothing).

    attach_sock: path to mirage-server's attach socket (MIRAGE_ATTACH_SOCK).
    timeout:     seconds to wait for the socket to appear and for "OK".
    """
    global _enabled
    if os.name != "posix" or platform.system() != "Linux":
        raise MirageTraceError("mirage_trace is only supported on Linux")
    with _lock:
        if _enabled:
            return
        _request_attach(attach_sock, timeout)  # steps 1+2: seize confirmed
        _install_trace_filter()                # step 3: only now
        _enabled = True


# --- step 1+2: the attach handshake ---------------------------------------


def _request_attach(attach_sock: str, timeout: float) -> None:
    """Send "ATTACH <pid>" to mirage-server and block until "OK".

    Retries the connect briefly: mirage-server creates the socket only after the
    client publishes the workspace, which may be slightly after the orchestrator
    calls enable().
    """
    deadline = _monotonic() + timeout
    sock = None
    last_err = None
    while _monotonic() < deadline:
        try:
            sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            sock.settimeout(max(0.1, deadline - _monotonic()))
            sock.connect(attach_sock)
            break
        except OSError as e:
            last_err = e
            if sock is not None:
                sock.close()
                sock = None
            _sleep(0.1)
    if sock is None:
        raise MirageTraceError(
            "could not connect to mirage-server attach socket %r: %s"
            % (attach_sock, last_err)
        )
    try:
        sock.sendall(b"ATTACH %d\n" % os.getpid())
        reply = _read_line(sock, deadline)
    finally:
        sock.close()
    if not reply.startswith("OK"):
        raise MirageTraceError("unexpected attach reply from mirage-server: %r" % reply)


def _read_line(sock: socket.socket, deadline: float) -> str:
    buf = b""
    while b"\n" not in buf:
        remaining = deadline - _monotonic()
        if remaining <= 0:
            raise MirageTraceError("timed out waiting for mirage-server to confirm attach")
        sock.settimeout(remaining)
        try:
            chunk = sock.recv(64)
        except OSError as e:
            raise MirageTraceError("error reading attach reply: %s" % e)
        if not chunk:
            raise MirageTraceError("mirage-server closed the connection before confirming attach")
        buf += chunk
    return buf.decode("ascii", "replace").strip()


# --- step 3: install the SECCOMP_RET_TRACE filter --------------------------


def _install_trace_filter() -> None:
    """Install the filter via libseccomp if available, else the ctypes
    fallback. Both set no_new_privs, trap the open + exec family with
    SECCOMP_RET_TRACE, TSYNC across threads, and ALLOW everything else."""
    try:
        import seccomp  # type: ignore
    except ImportError:
        _install_trace_filter_ctypes()
        return
    _install_trace_filter_libseccomp(seccomp)


def _install_trace_filter_libseccomp(seccomp) -> None:
    f = seccomp.SyscallFilter(defaction=seccomp.ALLOW)  # everything else runs normally
    f.set_attr(seccomp.Attr.CTL_TSYNC, 1)               # apply to ALL threads
    trapped = 0
    for name in _OPEN_SYSCALLS + _EXEC_SYSCALLS:
        try:
            f.add_rule(seccomp.TRACE(0), name)          # SECCOMP_RET_TRACE
            trapped += 1
        except seccomp.SeccompSyscallResolveError:
            pass  # syscall absent on this arch (e.g. open/creat on arm64)
    if trapped == 0:
        raise MirageTraceError("no open/exec syscalls resolved for this arch")
    f.load()  # sets no_new_privs + installs (SET_MODE_FILTER; no listener => coexists, no EBUSY)


# ctypes fallback: build the BPF program by hand and call seccomp(2) directly.
# Same shape as shim/launcher.c, swapping RET_USER_NOTIF -> RET_TRACE and adding
# the exec syscalls + the TSYNC flag.

# BPF instruction encodings.
_BPF_LD_W_ABS = 0x20  # BPF_LD | BPF_W | BPF_ABS
_BPF_JEQ_K = 0x15     # BPF_JMP | BPF_JEQ | BPF_K
_BPF_RET_K = 0x06     # BPF_RET | BPF_K

# seccomp_data field offsets: struct { int nr; __u32 arch; __u64 ip; __u64 args[6]; }
_OFF_NR = 0
_OFF_ARCH = 4

_SECCOMP_RET_ALLOW = 0x7FFF0000
_SECCOMP_RET_TRACE = 0x7FF00000

_SECCOMP_SET_MODE_FILTER = 1
_SECCOMP_FILTER_FLAG_TSYNC = 1
_PR_SET_NO_NEW_PRIVS = 38

# Per-arch: (AUDIT_ARCH, SYS_seccomp, {syscall_name: nr}). Only the syscalls we
# trap need to be listed.
_ARCH_X86_64 = (
    0xC000003E,
    317,
    {"open": 2, "openat": 257, "openat2": 437, "creat": 85, "execve": 59, "execveat": 322},
)
_ARCH_AARCH64 = (
    0xC00000B7,
    277,
    {"openat": 56, "openat2": 437, "execve": 221, "execveat": 281},
)


def _arch_profile():
    m = platform.machine().lower()
    if m in ("x86_64", "amd64"):
        return _ARCH_X86_64
    if m in ("aarch64", "arm64"):
        return _ARCH_AARCH64
    raise MirageTraceError("unsupported architecture for ctypes fallback: %r" % m)


def _bpf_stmt(code: int, k: int) -> bytes:
    # struct sock_filter { __u16 code; __u8 jt; __u8 jf; __u32 k; }
    return struct.pack("<HBBI", code, 0, 0, k & 0xFFFFFFFF)


def _bpf_jump(code: int, k: int, jt: int, jf: int) -> bytes:
    return struct.pack("<HBBI", code, jt, jf, k & 0xFFFFFFFF)


def _build_program(audit_arch: int, numbers) -> bytes:
    # Trap the syscalls present for this arch, in a stable order.
    nrs = [numbers[n] for n in (_OPEN_SYSCALLS + _EXEC_SYSCALLS) if n in numbers]
    n = len(nrs)
    if n == 0:
        raise MirageTraceError("no open/exec syscalls known for this arch")
    insns = [
        _bpf_stmt(_BPF_LD_W_ABS, _OFF_ARCH),
        _bpf_jump(_BPF_JEQ_K, audit_arch, 1, 0),
        _bpf_stmt(_BPF_RET_K, _SECCOMP_RET_ALLOW),  # foreign arch: run untouched
        _bpf_stmt(_BPF_LD_W_ABS, _OFF_NR),
    ]
    # For the i-th compare, jumping true must land on the RET_TRACE statement,
    # which sits right after the final RET_ALLOW: jt = n - i.
    for i, nr in enumerate(nrs):
        insns.append(_bpf_jump(_BPF_JEQ_K, nr, n - i, 0))
    insns.append(_bpf_stmt(_BPF_RET_K, _SECCOMP_RET_ALLOW))
    insns.append(_bpf_stmt(_BPF_RET_K, _SECCOMP_RET_TRACE))
    return b"".join(insns)


def _install_trace_filter_ctypes() -> None:
    import ctypes

    audit_arch, sys_seccomp, numbers = _arch_profile()
    prog_bytes = _build_program(audit_arch, numbers)
    insn_count = len(prog_bytes) // 8

    libc = ctypes.CDLL(None, use_errno=True)

    if libc.prctl(_PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0:
        err = ctypes.get_errno()
        raise MirageTraceError("prctl(PR_SET_NO_NEW_PRIVS) failed: %s" % os.strerror(err))

    filt = ctypes.create_string_buffer(prog_bytes, len(prog_bytes))

    class _SockFprog(ctypes.Structure):
        _fields_ = [("length", ctypes.c_ushort), ("filter", ctypes.c_void_p)]

    prog = _SockFprog(insn_count, ctypes.cast(filt, ctypes.c_void_p))

    libc.syscall.restype = ctypes.c_long
    rc = libc.syscall(
        ctypes.c_long(sys_seccomp),
        ctypes.c_long(_SECCOMP_SET_MODE_FILTER),
        ctypes.c_long(_SECCOMP_FILTER_FLAG_TSYNC),
        ctypes.byref(prog),
    )
    if rc != 0:
        err = ctypes.get_errno()
        raise MirageTraceError("seccomp(SET_MODE_FILTER, RET_TRACE) failed: %s" % os.strerror(err))


# --- small helpers (isolated so tests can monkeypatch time) ----------------


def _monotonic() -> float:
    import time

    return time.monotonic()


def _sleep(seconds: float) -> None:
    import time

    time.sleep(seconds)
