//go:build linux

// Package ptrace is the ptrace-based interception front-end for Mirage
// (docs/design-ptrace-interception.md), the alternative to the seccomp
// user-notification front-end. It is used when something else in the sandbox
// owns the one seccomp notification listener, or to avoid making mirage-server
// the workload's parent.
//
// mirage-server runs as a SIDE process with CAP_SYS_PTRACE. The workload's
// orchestrator installs a small seccomp filter returning SECCOMP_RET_TRACE for
// the open+exec family (see shim/trace-launcher.c / mirage_trace); this tracer
// PTRACE_SEIZEs the workload, receives a PTRACE_EVENT_SECCOMP stop on each such
// syscall, materializes the workspace file (reusing shim.Materializer), and
// resumes the syscall against now-real content (CONTINUE-after-materialize).
//
// ptrace requires every operation on a tracee to come from the SAME OS thread
// that attached it, so the whole loop runs on one locked thread.
package ptrace

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/suman724/mirage/internal/logging"
)

// ptrace requests and event/option constants not all exported by x/sys/unix.
const (
	ptraceSeize     = 0x4206
	ptraceInterrupt = 0x4207
	ptraceCont      = unix.PTRACE_CONT
	ptraceDetach    = unix.PTRACE_DETACH
	ptraceGetRegset = 0x4204
	ntPrStatus      = 1

	oTraceFork    = 0x00000002
	oTraceVfork   = 0x00000004
	oTraceClone   = 0x00000008
	oTraceExec    = 0x00000010
	oTraceSeccomp = 0x00000080
	// NOTE: PTRACE_O_EXITKILL is deliberately NOT set. This is a SIDE-ATTACH
	// front-end: mirage-server does not own the workload (the orchestrator
	// does), so the tracer dying must NOT take the workload down. Without
	// EXITKILL the kernel auto-detaches and resumes tracees if the tracer exits.
	seizeOptions = oTraceFork | oTraceVfork | oTraceClone | oTraceExec | oTraceSeccomp
	eventSeccomp = 7
	eventExec    = 4
	pathMaxBytes = 4096
)

// Materializer is the shim core this front-end drives (satisfied by
// *shim.Materializer). Keeps the dependency one-way and testable.
type Materializer interface {
	Root() string
	RelPath(abs string) (string, error)
	Ensure(rel string) error
}

// Stats is a snapshot of tracer counters.
type Stats struct {
	Traps     uint64 // PTRACE_EVENT_SECCOMP stops handled
	Workspace uint64 // those that resolved inside the workspace
	Errors    uint64
}

// Tracer seizes a workload and services its open/exec seccomp-trace stops.
type Tracer struct {
	mat Materializer
	log *slog.Logger

	traps     atomic.Uint64
	workspace atomic.Uint64
	errs      atomic.Uint64
	exitCode  atomic.Int32 // root workload exit code, -1 until it exits

	stopping atomic.Bool  // set by the ctx watcher to unwind the loop + detach
	tid      atomic.Int32 // OS thread id running the ptrace loop (for tgkill)
}

// New builds a Tracer over the given materializer.
func New(mat Materializer, logger *slog.Logger) (*Tracer, error) {
	if mat == nil {
		return nil, fmt.Errorf("ptrace: nil materializer")
	}
	t := &Tracer{mat: mat, log: logging.OrDefault(logger)}
	t.exitCode.Store(-1)
	return t, nil
}

// ExitCode returns the root workload's exit code, or -1 if it has not exited
// (or was killed by a signal).
func (t *Tracer) ExitCode() int { return int(t.exitCode.Load()) }

// Serve listens on the attach socket for one "ATTACH <pid>" request, seizes the
// target (and its threads), replies "OK", then runs the trace loop until the
// root tracee exits or ctx is cancelled. On ctx cancel it DETACHes from every
// tracee (leaving the workload running) and returns context.Canceled — it does
// NOT kill the workload, which the orchestrator owns. The whole ptrace
// lifecycle is pinned to one OS thread.
func (t *Tracer) Serve(ctx context.Context, attachSock string) error {
	_ = os.Remove(attachSock)
	ln, err := net.Listen("unix", attachSock)
	if err != nil {
		return fmt.Errorf("ptrace: listen %q: %w", attachSock, err)
	}
	defer ln.Close()
	if err := os.Chmod(filepath.Dir(attachSock), 0o700); err != nil {
		t.log.Warn("restrict attach socket dir", "err", err)
	}
	t.log.Info("ptrace tracer waiting for attach", "socket", attachSock, "root", t.mat.Root())

	// Unblock Accept if the client disconnects before anyone attaches.
	acceptDone := make(chan struct{})
	defer close(acceptDone)
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-acceptDone:
		}
	}()

	conn, err := ln.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ptrace: accept: %w", err)
	}
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("ptrace: read attach request: %w", err)
	}
	verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	if verb != "ATTACH" {
		return fmt.Errorf("ptrace: unexpected request %q", verb)
	}
	rootPid, err := strconv.Atoi(arg)
	if err != nil {
		return fmt.Errorf("ptrace: bad pid %q: %w", arg, err)
	}

	// All ptrace ops + waits must run on the thread that seizes — pin it.
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		t.tid.Store(int32(unix.Gettid()))

		traced := map[int]bool{}
		if err := t.seizeAll(rootPid, traced); err != nil {
			result <- err
			return
		}
		// Ack only after the seize succeeds (ordering: tracer attached before
		// the orchestrator installs the RET_TRACE filter — avoids ENOSYS).
		if _, werr := conn.Write([]byte("OK\n")); werr != nil {
			t.log.Warn("ack attach", "err", werr)
		}

		// On ctx cancel, flag the loop and wake it out of wait4 by signalling
		// our own thread. SIGURG is handled by the Go runtime (async preemption),
		// so it interrupts the blocking syscall with EINTR without terminating us.
		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-ctx.Done():
				t.stopping.Store(true)
				_ = unix.Tgkill(unix.Getpid(), int(t.tid.Load()), syscall.SIGURG)
			case <-watchDone:
			}
		}()

		result <- t.loop(rootPid, traced)
	}()
	return <-result
}

// seizeAll seizes the thread-group leader and every existing thread, recording
// each into traced.
func (t *Tracer) seizeAll(pid int, traced map[int]bool) error {
	if err := seize(pid); err != nil {
		return fmt.Errorf("ptrace: seize %d: %w", pid, err)
	}
	traced[pid] = true
	// Seize sibling threads (re-scan once to reduce the create-during-loop race;
	// TRACECLONE catches the rest).
	for pass := 0; pass < 2; pass++ {
		tids, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
		for _, d := range tids {
			tid, err := strconv.Atoi(d.Name())
			if err != nil || tid == pid {
				continue
			}
			if seize(tid) == nil { // best-effort; already-seized returns EPERM
				traced[tid] = true
			}
		}
	}
	return nil
}

// loop is the wait/dispatch loop (runs on the locked thread). traced tracks the
// live tracee set so a ctx-cancel teardown can detach all of them.
func (t *Tracer) loop(rootPid int, traced map[int]bool) error {
	for {
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(-1, &ws, unix.WALL, nil)
		if err == unix.EINTR {
			if t.stopping.Load() {
				t.detachAll(traced)
				return context.Canceled
			}
			continue
		}
		if err == unix.ECHILD {
			return nil // all tracees gone
		}
		if err != nil {
			return fmt.Errorf("ptrace: wait4: %w", err)
		}

		switch {
		case ws.Exited() || ws.Signaled():
			delete(traced, wpid)
			if wpid == rootPid {
				if ws.Exited() {
					t.exitCode.Store(int32(ws.ExitStatus()))
				}
				return nil // root workload finished
			}
		case ws.Stopped():
			traced[wpid] = true
			t.onStop(wpid, ws)
		}
	}
}

// detachAll stops every tracee and PTRACE_DETACHes it, leaving the workload
// running natively (no more traps). Used on client disconnect: we relinquish
// interception without disturbing the orchestrator's process tree.
func (t *Tracer) detachAll(traced map[int]bool) {
	for pid := range traced {
		_ = interrupt(pid) // request a ptrace-stop; best-effort
	}
	// Drain: each tracee reports a stop (or has already exited); detach on stop.
	for len(traced) > 0 {
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(-1, &ws, unix.WALL, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return // ECHILD or fatal — nothing left to detach
		}
		switch {
		case ws.Exited() || ws.Signaled():
			delete(traced, wpid)
		case ws.Stopped():
			_ = detach(wpid) // resume natively, no signal injected
			delete(traced, wpid)
		}
	}
}

// onStop dispatches a ptrace stop on the locked thread. The ptrace event (if
// any) is in bits 16-23 of the raw status.
func (t *Tracer) onStop(pid int, ws unix.WaitStatus) {
	sig := ws.StopSignal()
	event := (int(ws) >> 16) & 0xff

	switch {
	case sig == unix.SIGTRAP && event == eventSeccomp:
		t.traps.Add(1)
		t.handleSeccomp(pid) // materialize before resuming the syscall
		_ = ptraceContinue(pid, 0)
	case sig == unix.SIGTRAP && event != 0:
		// fork/vfork/clone/exec/exit event — child auto-traced; just continue.
		_ = ptraceContinue(pid, 0)
	case sig == unix.SIGTRAP || sig == unix.SIGSTOP:
		// initial/group-stop or a plain trap — continue without injecting a signal.
		_ = ptraceContinue(pid, 0)
	default:
		// Deliver any other signal transparently to the tracee.
		_ = ptraceContinue(pid, int(sig))
	}
}

// handleSeccomp services one open/exec stop: read the path, materialize if it's
// a workspace file. The syscall is then resumed (continue) by the caller.
func (t *Tracer) handleSeccomp(pid int) {
	regs, err := getRegs(pid)
	if err != nil {
		t.errs.Add(1)
		t.log.Debug("getregs failed", "pid", pid, "err", err)
		return
	}
	call, ok := decodeSyscall(sysNR(regs))
	if !ok {
		return // not an open/exec we trap (filter/decoder mismatch) — pass through
	}
	raw, err := readCString(pid, arg(regs, call.pathArg), pathMaxBytes)
	if err != nil || raw == "" {
		t.log.Debug("path read failed", "pid", pid, "err", err)
		return
	}
	abs, err := resolvePath(pid, raw, regs, call)
	if err != nil {
		t.log.Debug("path resolve failed", "pid", pid, "path", raw, "err", err)
		return
	}
	rel, err := t.mat.RelPath(abs)
	if err != nil {
		return // outside the workspace — nothing to do
	}
	t.workspace.Add(1)
	if err := t.mat.Ensure(rel); err != nil {
		t.errs.Add(1)
		t.log.Error("materialize failed", "path", rel, "err", err)
	}
}

// resolvePath turns the raw path argument into an absolute path, handling *at
// dirfd / AT_FDCWD relative paths via /proc/<pid>/{fd,cwd}.
func resolvePath(pid int, raw string, regs []byte, call openCall) (string, error) {
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), nil
	}
	var base string
	var err error
	if call.dirfdArg >= 0 {
		dirfd := int32(arg(regs, call.dirfdArg))
		if dirfd == unix.AT_FDCWD {
			base, err = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		} else {
			base, err = os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, dirfd))
		}
	} else {
		base, err = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	}
	if err != nil {
		return "", fmt.Errorf("resolve base: %w", err)
	}
	return filepath.Clean(filepath.Join(base, raw)), nil
}

// Stats snapshots the counters.
func (t *Tracer) Stats() Stats {
	return Stats{Traps: t.traps.Load(), Workspace: t.workspace.Load(), Errors: t.errs.Load()}
}

// --- ptrace syscall helpers (must be called on the seizing thread) ---

func seize(pid int) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, ptraceSeize, uintptr(pid), 0, seizeOptions, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func ptraceContinue(pid, sig int) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, ptraceCont, uintptr(pid), 0, uintptr(sig), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// interrupt requests a ptrace-stop on a running SEIZEd tracee (PTRACE_INTERRUPT).
func interrupt(pid int) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, ptraceInterrupt, uintptr(pid), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// detach releases a stopped tracee, which then resumes running untraced.
func detach(pid int) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, ptraceDetach, uintptr(pid), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// getRegs fetches the general-purpose register set via PTRACE_GETREGSET into a
// raw buffer; arch files decode syscall nr and args by offset.
func getRegs(pid int) ([]byte, error) {
	buf := make([]byte, 512)
	iov := unix.Iovec{Base: &buf[0], Len: uint64(len(buf))}
	_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, ptraceGetRegset, uintptr(pid),
		ntPrStatus, uintptr(unsafe.Pointer(&iov)), 0, 0)
	if errno != 0 {
		return nil, errno
	}
	return buf[:iov.Len], nil
}

// readCString reads a NUL-terminated string of at most max bytes from another
// process's memory via /proc/<pid>/mem (allowed cross-process with CAP_SYS_PTRACE).
func readCString(pid int, addr uint64, max int) (string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, max)
	n, _ := f.ReadAt(buf, int64(addr))
	if n == 0 {
		return "", fmt.Errorf("read proc mem at %#x", addr)
	}
	buf = buf[:n]
	if i := indexByte(buf, 0); i >= 0 {
		return string(buf[:i]), nil
	}
	return "", fmt.Errorf("unterminated path")
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
