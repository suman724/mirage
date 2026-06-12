#!/usr/bin/env python3
"""
Shimmer issue #8 — seccomp user-notification viability probe (Fargate).

This answers ONE question that no amount of local testing or research can:
does the ECS Fargate runtime permit the seccomp() mechanism Shimmer would use
to intercept open() for *all* binaries (including Go/static)? See
docs/design-shimmer.md §11 and docs/mirage-on-fargate.md §7.

It is a PROBE, not the product. The real supervisor is Go, integrated with the
chunk store; this just exercises the kernel/runtime primitives that could
plausibly differ on Fargate. A PASS is the green light to build those
components; a FAIL says fall back to the exec gate (S3) on the C shim we
already validated.

WHAT IT CHECKS (the minimum that is actually trustworthy):
  1. install   — PR_SET_NO_NEW_PRIVS + seccomp(SECCOMP_FILTER_FLAG_NEW_LISTENER)
                 returns a listener fd. (The headline unknown: does the runtime
                 let an unprivileged process install a notification filter?)
  2. notify    — a real openat() in the target traps and NOTIF_RECV delivers it.
  3. mem-read  — the servicer reads the target's path argument out of
                 /proc/<pid>/mem, bracketed by NOTIF_ID_VALID (the man-page TOCTOU
                 rule). This is the SECOND Fargate unknown: a hardened
                 ptrace_scope can permit seccomp() yet forbid cross-process
                 memory reads — and every materialization decision needs the path.
  4. respond   — NOTIF_SEND hands back a synthetic result and the target resumes.

It deliberately does NOT test exec-inheritance, many-process concurrency, or
per-trap latency: those are load-bearing for the product but behave identically
on any kernel, so they are proven later against the real Go supervisor, not on
a scarce Fargate run.

TOPOLOGY: parent process = servicer (reads child memory); forked child =
target (installs the filter on itself, triggers one openat). The Jupyter
kernel is the parent's parent and is NEVER filtered — safe to run in a cell.
Note: parent is an ANCESTOR of the target, the most permissive ptrace case; if
Fargate's ptrace_scope is 1, production must likewise keep the supervisor an
ancestor of the workload, and if it is >=2 even this read fails (which the
probe will report).

Designed to paste into one Jupyter cell on a Fargate task. Pure stdlib, no
build step. Python >= 3.9 (socket.send_fds/recv_fds).
"""

import ctypes
import fcntl
import os
import platform
import select
import socket
import struct
import sys

# --- syscall numbers (arch-specific; covers x86_64 and Graviton/arm64) -------
#
# We trap mkdirat, NOT open/openat, deliberately. The probe needs a syscall
# with a userspace pathname pointer (to prove the cross-process memory read),
# but it must be one CPython never emits on its own: right after the filter is
# installed, the child still runs Python (send_fds, etc.), and if that
# machinery made a *filtered* syscall it would trap and block — deadlocking
# against the servicer, which is itself waiting to receive the listener fd.
# Nothing in CPython calls mkdirat incidentally, so the only trap is the one we
# fire on purpose. open() interception is the production target; this is a probe.
_MACH = platform.machine()
if _MACH in ("x86_64", "amd64"):
    SYS_seccomp, SYS_TRAP = 317, 258  # mkdirat
elif _MACH in ("aarch64", "arm64"):
    SYS_seccomp, SYS_TRAP = 277, 34   # mkdirat
else:
    print(f"UNSUPPORTED arch {_MACH!r}; add its syscall numbers", file=sys.stderr)
    sys.exit(2)

# --- constants ---------------------------------------------------------------
PR_SET_NO_NEW_PRIVS = 38
SECCOMP_SET_MODE_FILTER = 1
SECCOMP_GET_NOTIF_SIZES = 3
SECCOMP_FILTER_FLAG_NEW_LISTENER = 0x08
SECCOMP_RET_USER_NOTIF = 0x7FC00000
SECCOMP_RET_ALLOW = 0x7FFF0000

# ioctl numbers on the listener fd. x/sys/unix exports RECV/SEND but not
# ID_VALID, so it is computed here via _IOW('!', 2, __u64) (see seccomp.h).
SECCOMP_IOCTL_NOTIF_RECV = 0xC0502100      # _IOWR('!', 0, struct seccomp_notif)
SECCOMP_IOCTL_NOTIF_SEND = 0xC0182101      # _IOWR('!', 1, struct seccomp_notif_resp)
SECCOMP_IOCTL_NOTIF_ID_VALID = 0x40082102  # _IOW ('!', 2, __u64)

AT_FDCWD = -100
ENOENT = 2
SENTINEL = b"/MIRAGE_SECCOMP_PROBE_SENTINEL"

# struct seccomp_notif { u64 id; u32 pid; u32 flags; seccomp_data data; }
# seccomp_data { int nr; u32 arch; u64 ip; u64 args[6]; }  => notif is 80 bytes.
NOTIF_SIZE_FALLBACK = 80
RESP_SIZE = 24                              # u64 id; s64 val; s32 error; u32 flags
ARGS_OFFSET = 32                            # args[0] within seccomp_notif
PID_OFFSET = 8
ID_OFFSET = 0
NR_OFFSET = 16


def _libc():
    libc = ctypes.CDLL(None, use_errno=True)
    libc.syscall.restype = ctypes.c_long
    libc.prctl.restype = ctypes.c_int
    return libc


def _notif_size(libc):
    """Ask the kernel the notification struct size (forward-compatible)."""
    buf = bytearray(6)  # struct seccomp_notif_sizes { u16 notif, notif_resp, data }
    arr = (ctypes.c_char * 6).from_buffer(buf)
    rc = libc.syscall(ctypes.c_long(SYS_seccomp),
                      ctypes.c_long(SECCOMP_GET_NOTIF_SIZES),
                      ctypes.c_long(0),
                      ctypes.cast(arr, ctypes.c_void_p))
    if rc == 0:
        notif, _resp, _data = struct.unpack("HHH", buf)
        return max(notif, NOTIF_SIZE_FALLBACK)
    return NOTIF_SIZE_FALLBACK


# --------------------------------------------------------------------------- #
# Target side (forked child): install the filter on itself, hand the listener  #
# fd to the servicer, then make exactly one openat() that traps.               #
# --------------------------------------------------------------------------- #
def _run_target(sock):
    try:
        _run_target_inner(sock)
    except BaseException as e:  # noqa: BLE001 - surface child crash to the parent
        try:
            socket.send_fds(sock, [b"FAIL child_exception: %r" % e], [])
        except Exception:
            pass
    finally:
        os._exit(0)


def _run_target_inner(sock):
    libc = _libc()

    # 1. Drop the ability to gain privileges — required to install a filter
    #    without CAP_SYS_ADMIN. This is the unprivileged path Fargate must allow.
    if libc.prctl(ctypes.c_int(PR_SET_NO_NEW_PRIVS), ctypes.c_ulong(1), 0, 0, 0) != 0:
        socket.send_fds(sock, [b"FAIL no_new_privs errno=%d" % ctypes.get_errno()], [])
        return

    # BPF: trap syscall -> USER_NOTIF (to servicer), everything else -> ALLOW.
    filt = b"".join([
        struct.pack("HBBI", 0x20, 0, 0, 0),                  # ld  [0] (syscall nr)
        struct.pack("HBBI", 0x15, 0, 1, SYS_TRAP),           # jeq trap ? : +1
        struct.pack("HBBI", 0x06, 0, 0, SECCOMP_RET_USER_NOTIF),
        struct.pack("HBBI", 0x06, 0, 0, SECCOMP_RET_ALLOW),
    ])
    filt_buf = ctypes.create_string_buffer(filt, len(filt))

    class SockFprog(ctypes.Structure):
        _fields_ = [("len", ctypes.c_ushort), ("filter", ctypes.c_void_p)]

    prog = SockFprog(len(filt) // 8, ctypes.cast(filt_buf, ctypes.c_void_p))

    # 2. Install. A returned fd is the headline signal.
    ctypes.set_errno(0)
    lfd = libc.syscall(ctypes.c_long(SYS_seccomp),
                       ctypes.c_long(SECCOMP_SET_MODE_FILTER),
                       ctypes.c_long(SECCOMP_FILTER_FLAG_NEW_LISTENER),
                       ctypes.byref(prog))
    if lfd < 0:
        socket.send_fds(sock, [b"FAIL seccomp_install errno=%d" % ctypes.get_errno()], [])
        return

    # Hand the listener to the servicer (sendmsg is ALLOWed by our own filter).
    socket.send_fds(sock, [b"OK"], [int(lfd)])

    # 3. The trap. Blocks here until the servicer responds. We pass a known
    #    sentinel path so the servicer can prove it read OUR memory correctly.
    #    args: (dirfd, pathname, mode) — pathname is arg[1], same as open*.
    path = ctypes.create_string_buffer(SENTINEL + b"\x00")
    libc.syscall(ctypes.c_long(SYS_TRAP),
                 ctypes.c_long(AT_FDCWD),
                 ctypes.c_void_p(ctypes.addressof(path)),
                 ctypes.c_long(0))


# --------------------------------------------------------------------------- #
# Servicer side (parent): receive the listener, service one notification.      #
# --------------------------------------------------------------------------- #
def _id_valid(lfd, notif_id):
    try:
        fcntl.ioctl(lfd, SECCOMP_IOCTL_NOTIF_ID_VALID, struct.pack("Q", notif_id))
        return True
    except OSError:
        return False


def _service(lfd, notif_size, result):
    # 2. Receive the notification (with a timeout so a stuck target can't hang).
    ready, _, _ = select.select([lfd], [], [], 10)
    if not ready:
        result["error"] = "no notification within 10s (target never trapped?)"
        return
    buf = bytearray(notif_size)
    fcntl.ioctl(lfd, SECCOMP_IOCTL_NOTIF_RECV, buf, True)
    notif_id = struct.unpack_from("Q", buf, ID_OFFSET)[0]
    pid = struct.unpack_from("I", buf, PID_OFFSET)[0]
    nr = struct.unpack_from("i", buf, NR_OFFSET)[0]
    path_ptr = struct.unpack_from("Q", buf, ARGS_OFFSET + 8 * 1)[0]  # pathname arg1
    result["notify"] = (nr == SYS_TRAP)
    result["target_pid"] = pid

    # 3. Cross-process memory read, ID_VALID-bracketed (man-page TOCTOU rule).
    if not _id_valid(lfd, notif_id):
        result["error"] = "notification id invalid before read"
        return
    try:
        mfd = os.open(f"/proc/{pid}/mem", os.O_RDONLY)
        try:
            raw = os.pread(mfd, len(SENTINEL) + 1, path_ptr)
        finally:
            os.close(mfd)
        got = raw.split(b"\x00", 1)[0]
        result["mem_read"] = True
        result["mem_match"] = (got == SENTINEL)
        if got != SENTINEL:
            result["error"] = f"path mismatch: {got!r}"
    except OSError as e:
        # THE ptrace_scope signal: install allowed but memory read denied.
        result["mem_read"] = False
        result["error"] = f"/proc/{pid}/mem read failed: {e}"

    if not _id_valid(lfd, notif_id):  # re-check before using/responding
        result["error"] = "notification id invalidated during read"
        return

    # 4. Respond: synthetic ENOENT (no real open happens, no side effects).
    resp = struct.pack("QqiI", notif_id, 0, -ENOENT, 0)
    fcntl.ioctl(lfd, SECCOMP_IOCTL_NOTIF_SEND, bytearray(resp), True)
    result["respond"] = True


def probe():
    """Run the full probe. Returns a result dict; never raises in normal flow."""
    libc = _libc()
    result = {
        "arch": _MACH,
        "kernel": platform.release(),
        "ptrace_scope": _read_ptrace_scope(),
        "install": False, "notify": False, "mem_read": False,
        "mem_match": False, "respond": False, "error": None,
    }
    notif_size = _notif_size(libc)

    parent_sock, child_sock = socket.socketpair(socket.AF_UNIX, socket.SOCK_DGRAM)
    pid = os.fork()
    if pid == 0:
        parent_sock.close()
        try:
            _run_target(child_sock)
        finally:
            os._exit(0)

    # --- parent / servicer ---
    child_sock.close()
    parent_sock.settimeout(15)  # never block forever if the child died early
    try:
        msg, fds, _flags, _addr = socket.recv_fds(parent_sock, 256, 1)
        if not fds:
            result["error"] = "target reported: " + msg.decode(errors="replace")
        else:
            result["install"] = True
            lfd = fds[0]
            try:
                _service(lfd, notif_size, result)
            finally:
                # Closing the listener releases the target's trapped syscall
                # (it resumes with ENOSYS) even if we never responded.
                os.close(lfd)
    except socket.timeout:
        result["error"] = "timed out waiting for the target to install + send the listener"
    except Exception as e:  # noqa: BLE001 - probe must always report, not crash
        result["error"] = f"servicer exception: {e!r}"
    finally:
        parent_sock.close()
        _reap(pid)
    return result


def _reap(pid):
    """Wait for the child, but never hang: SIGKILL it if it overstays."""
    for _ in range(30):  # ~3s
        try:
            done, _ = os.waitpid(pid, os.WNOHANG)
        except ChildProcessError:
            return
        if done:
            return
        select.select([], [], [], 0.1)
    try:
        os.kill(pid, 9)
        os.waitpid(pid, 0)
    except (ChildProcessError, ProcessLookupError):
        pass


def _read_ptrace_scope():
    try:
        with open("/proc/sys/kernel/yama/ptrace_scope") as f:
            return f.read().strip()
    except OSError:
        return "n/a"  # yama not present => effectively 0


def _report(r):
    ok = r["install"] and r["notify"] and r["mem_read"] and r["mem_match"] and r["respond"]
    line = "=" * 60
    print(line)
    print("SECCOMP UNOTIFY PROBE  (Shimmer #8)")
    print(line)
    print(f"  arch            : {r['arch']}")
    print(f"  kernel          : {r['kernel']}")
    print(f"  ptrace_scope    : {r['ptrace_scope']}")
    print("  ---")
    print(f"  1 install       : {'PASS' if r['install'] else 'FAIL'}  (seccomp NEW_LISTENER)")
    print(f"  2 notify        : {'PASS' if r['notify'] else 'FAIL'}  (trap syscall + NOTIF_RECV)")
    print(f"  3 mem-read      : {'PASS' if r['mem_read'] else 'FAIL'}  (/proc/<pid>/mem path read)")
    print(f"    mem-match     : {'PASS' if r['mem_match'] else 'FAIL'}  (path == sentinel)")
    print(f"  4 respond       : {'PASS' if r['respond'] else 'FAIL'}  (NOTIF_SEND, target resumed)")
    if r["error"]:
        print(f"  note            : {r['error']}")
    print(line)
    print(f"  VERDICT         : {'PASS — seccomp foundation is viable here' if ok else 'FAIL — see note above'}")
    print(line)
    return ok


if __name__ == "__main__":
    sys.exit(0 if _report(probe()) else 1)
