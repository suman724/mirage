//go:build linux

// Package seccomp is the syscall-level interception front-end for Shimmer
// (docs/design-shimmer.md §3.3), the primary mechanism on platforms that forbid
// FUSE (e.g. Fargate). A small C launcher (shim/launcher.c) installs a seccomp
// user-notification filter trapping the open family and hands the listener fd
// here; this supervisor services each trapped open by materializing the file
// (through the same shim.Materializer the LD_PRELOAD front-end uses) and then
// letting the kernel run the real open against now-real content.
//
// Unlike the LD_PRELOAD shim this covers EVERY binary — libc, Go, static — at
// the syscall boundary, which nothing in userspace can bypass.
//
// Response strategy (this slice): CONTINUE-after-materialize. We materialize
// the workspace file, then respond with SECCOMP_USER_NOTIF_FLAG_CONTINUE so the
// kernel re-runs the tool's original open against the now-real file — correct,
// and free of fd/flag-matching edge cases. ADDFD (race-free fd injection) is the
// documented hardening follow-up; the materialize step is identical either way.
package seccomp

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/suman724/mirage/internal/logging"
)

// seccomp notification ioctls on the listener fd. x/sys/unix exports the
// SECCOMP_* flags but not these ioctl request numbers, so they are fixed here
// (asm-generic _IOC encoding, identical on amd64 and arm64).
const (
	ioctlNotifRecv    = 0xc0502100 // _IOWR('!', 0, struct seccomp_notif)
	ioctlNotifSend    = 0xc0182101 // _IOWR('!', 1, struct seccomp_notif_resp)
	ioctlNotifIDValid = 0x40082102 // _IOW ('!', 2, __u64)

	userNotifFlagContinue = 0x1 // SECCOMP_USER_NOTIF_FLAG_CONTINUE
	seccompGetNotifSizes  = 0x3 // SECCOMP_GET_NOTIF_SIZES

	// struct seccomp_notif field offsets (id u64; pid u32; flags u32;
	// seccomp_data{ nr s32; arch u32; ip u64; args[6] u64 }). Total 80 bytes.
	offID    = 0
	offPid   = 8
	offNR    = 16
	offArgs  = 32
	notifLen = 80
	respLen  = 24 // id u64; val s64; error s32; flags u32

	pathMax = 4096
)

// Materializer is the shim core this front-end drives. *shim.Materializer
// satisfies it; the interface keeps the dependency one-way and testable.
type Materializer interface {
	Root() string
	RelPath(abs string) (string, error)
	Ensure(rel string) error
	Dirty(rel string) error
}

// Stats is a snapshot of supervisor counters.
type Stats struct {
	Traps        uint64 // notifications received
	Workspace    uint64 // opens that resolved inside the workspace
	Materialized uint64 // Ensure calls that did real work is counted by the table; this is workspace opens serviced
	FastPath     uint64 // opens outside the workspace, passed straight through
	Errors       uint64
}

// Supervisor services seccomp open-notifications over one listener fd.
type Supervisor struct {
	mat       Materializer
	log       *slog.Logger
	notifSize int

	traps     atomic.Uint64
	workspace atomic.Uint64
	fastpath  atomic.Uint64
	errs      atomic.Uint64

	stop atomic.Bool
}

// New builds a Supervisor over the given materializer, querying the kernel's
// notification struct size for forward compatibility.
func New(mat Materializer, logger *slog.Logger) (*Supervisor, error) {
	if mat == nil {
		return nil, fmt.Errorf("seccomp: nil materializer")
	}
	return &Supervisor{
		mat:       mat,
		log:       logging.OrDefault(logger),
		notifSize: notifSizeFromKernel(),
	}, nil
}

// notifSizeFromKernel returns sizeof(struct seccomp_notif), preferring the
// kernel's reported value (>= our compiled 80) so a future-larger struct does
// not overflow the receive buffer.
func notifSizeFromKernel() int {
	var sizes [3]uint16 // { seccomp_notif, seccomp_notif_resp, seccomp_data }
	r, _, errno := syscall.Syscall(uintptr(unix.SYS_SECCOMP), seccompGetNotifSizes, 0,
		uintptr(unsafe.Pointer(&sizes[0])))
	if errno == 0 && r == 0 && int(sizes[0]) >= notifLen {
		return int(sizes[0])
	}
	return notifLen
}

// Serve services notifications until Stop is called. A SINGLE receiver goroutine
// owns NOTIF_RECV (the seccomp listener does not reliably support concurrent or
// non-blocking RECV, so multiple receivers would block); it dispatches each
// notification to a pool of `workers` handler goroutines, so a slow
// materialization (a network chunk fault) does not stall the receiver. Stop is
// honored via a poll timeout, since closing an fd does not wake a blocked ioctl.
func (s *Supervisor) Serve(listenerFd, workers int) error {
	if workers < 1 {
		workers = 1
	}
	// Non-blocking so RECV returns EAGAIN instead of blocking when poll
	// reported POLLHUP (last filtered process exited) rather than a real
	// pending notification — otherwise the receiver wedges in RECV and never
	// rechecks Stop.
	if err := unix.SetNonblock(listenerFd, true); err != nil {
		return fmt.Errorf("seccomp: set listener non-blocking: %w", err)
	}
	s.log.Info("seccomp supervisor servicing", "listener_fd", listenerFd,
		"workers", workers, "root", s.mat.Root())

	ch := make(chan []byte, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for buf := range ch {
				s.handle(listenerFd, buf)
			}
		}()
	}

	err := s.receive(listenerFd, ch)
	s.log.Debug("seccomp receiver returned; draining handlers", "err", err)
	close(ch)
	wg.Wait()
	s.log.Debug("seccomp handlers drained")
	return err
}

// Stop signals the receiver to finish. Serve returns once handlers drain.
func (s *Supervisor) Stop() { s.stop.Store(true) }

// receive is the sole NOTIF_RECV caller: poll for a pending notification, read
// it into a fresh buffer, hand it to a worker. Because it only RECVs after poll
// reports POLLIN, the RECV never blocks even though the fd is blocking.
func (s *Supervisor) receive(fd int, ch chan<- []byte) error {
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if s.stop.Load() {
			return nil
		}
		pfd[0].Revents = 0
		n, err := unix.Poll(pfd, 200) // 200ms: bounds Stop latency
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return fmt.Errorf("seccomp: poll listener: %w", err)
		}
		if n == 0 {
			continue // timeout; re-check stop
		}
		// Only RECV on a real pending notification. After the last filtered
		// process exits, poll reports POLLHUP (not POLLIN); RECV would then
		// block (the seccomp listener ignores O_NONBLOCK), wedging the receiver
		// past the Stop check. Skipping non-POLLIN wake-ups avoids that.
		if pfd[0].Revents&unix.POLLIN == 0 {
			continue
		}
		buf := make([]byte, s.notifSize) // fresh (zeroed) per notification
		if err := ioctlPtr(fd, ioctlNotifRecv, unsafe.Pointer(&buf[0])); err != nil {
			switch err {
			case syscall.EAGAIN, syscall.EINTR, syscall.ENOENT:
				continue // nothing ready / notification vanished
			default:
				if s.stop.Load() {
					return nil
				}
				return fmt.Errorf("seccomp: NOTIF_RECV: %w", err)
			}
		}
		s.traps.Add(1)
		ch <- buf
	}
}

// handle services one trapped open: resolve the path, decide workspace vs.
// pass-through, materialize if needed, then respond.
func (s *Supervisor) handle(fd int, buf []byte) {
	id := binary.LittleEndian.Uint64(buf[offID:])
	pid := binary.LittleEndian.Uint32(buf[offPid:])
	nr := int32(binary.LittleEndian.Uint32(buf[offNR:]))
	var args [6]uint64
	for i := 0; i < 6; i++ {
		args[i] = binary.LittleEndian.Uint64(buf[offArgs+i*8:])
	}

	call, ok := decodeOpen(nr)
	if !ok {
		// We only asked the kernel to trap the open family; anything else is a
		// filter/decoder mismatch. Pass it through rather than wedging the tool.
		s.continueSyscall(fd, id)
		return
	}

	abs, err := s.resolvePath(int(pid), id, fd, args, call)
	if err != nil {
		// Could not read the path (target raced/exited, or memory unreadable):
		// let the kernel run the real syscall, which will produce the right errno.
		s.log.Debug("path resolution failed; passing through", "pid", pid, "err", err)
		s.continueSyscall(fd, id)
		return
	}

	rel, err := s.mat.RelPath(abs)
	if err != nil {
		// Outside the workspace (the common case: /usr/lib, /etc, …). Pass through.
		s.fastpath.Add(1)
		s.continueSyscall(fd, id)
		return
	}

	s.workspace.Add(1)
	if err := s.mat.Ensure(rel); err != nil {
		s.errs.Add(1)
		s.log.Error("ensure failed; failing the open", "path", rel, "err", err)
		s.errorSyscall(fd, id, int32(unix.EIO))
		return
	}
	if call.writeIntent(args, int(pid)) {
		if err := s.mat.Dirty(rel); err != nil {
			s.log.Warn("dirty tracking failed", "path", rel, "err", err)
		}
	}
	// Content is now real on disk; let the kernel run the tool's original open.
	s.continueSyscall(fd, id)
}

// resolvePath reads the path argument from the target's memory and resolves it
// to an absolute path, handling *at dirfd / AT_FDCWD relative paths. Every read
// is bracketed by NOTIF_ID_VALID (the man-page TOCTOU rule).
func (s *Supervisor) resolvePath(pid int, id uint64, listenerFd int, args [6]uint64, call openCall) (string, error) {
	if !s.idValid(listenerFd, id) {
		return "", fmt.Errorf("notification %d invalid before read", id)
	}
	raw, err := readCString(pid, args[call.pathArg], pathMax)
	if err != nil {
		return "", err
	}
	if !s.idValid(listenerFd, id) {
		return "", fmt.Errorf("notification %d invalidated during read", id)
	}
	if raw == "" {
		return "", fmt.Errorf("empty path")
	}

	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), nil
	}
	// Relative: resolve against the dirfd (openat/openat2) or cwd.
	var base string
	if call.dirfdArg >= 0 {
		dirfd := int32(args[call.dirfdArg])
		if dirfd == unix.AT_FDCWD {
			base, err = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		} else {
			base, err = os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, dirfd))
		}
	} else {
		base, err = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	}
	if err != nil {
		return "", fmt.Errorf("resolve dir base: %w", err)
	}
	return filepath.Clean(filepath.Join(base, raw)), nil
}

func (s *Supervisor) idValid(fd int, id uint64) bool {
	return ioctlPtr(fd, ioctlNotifIDValid, unsafe.Pointer(&id)) == nil
}

// continueSyscall tells the kernel to run the target's original syscall.
func (s *Supervisor) continueSyscall(fd int, id uint64) {
	resp := make([]byte, respLen)
	binary.LittleEndian.PutUint64(resp[0:], id)
	binary.LittleEndian.PutUint32(resp[20:], userNotifFlagContinue)
	if err := ioctlPtr(fd, ioctlNotifSend, unsafe.Pointer(&resp[0])); err != nil {
		// Target may have been killed; the notification is then already gone.
		s.log.Debug("NOTIF_SEND(continue) failed", "id", id, "err", err)
	}
}

// errorSyscall makes the target's syscall fail with errno (fail loud).
func (s *Supervisor) errorSyscall(fd int, id uint64, errno int32) {
	resp := make([]byte, respLen)
	binary.LittleEndian.PutUint64(resp[0:], id)
	binary.LittleEndian.PutUint32(resp[16:], uint32(-errno)) // error field (negated)
	if err := ioctlPtr(fd, ioctlNotifSend, unsafe.Pointer(&resp[0])); err != nil {
		s.log.Debug("NOTIF_SEND(error) failed", "id", id, "err", err)
	}
}

// Stats snapshots the counters.
func (s *Supervisor) Stats() Stats {
	return Stats{
		Traps:        s.traps.Load(),
		Workspace:    s.workspace.Load(),
		Materialized: s.workspace.Load(),
		FastPath:     s.fastpath.Load(),
		Errors:       s.errs.Load(),
	}
}

// ioctlPtr issues ioctl(fd, req, ptr), translating a zero errno to nil.
func ioctlPtr(fd int, req uintptr, ptr unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(uintptr(unix.SYS_IOCTL), uintptr(fd), req, uintptr(ptr))
	if errno != 0 {
		return errno
	}
	return nil
}

// readCString reads a NUL-terminated string of at most max bytes from another
// process's memory at addr (via /proc/<pid>/mem).
func readCString(pid int, addr uint64, max int) (string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return "", fmt.Errorf("open proc mem: %w", err)
	}
	defer f.Close()
	buf := make([]byte, max)
	n, err := f.ReadAt(buf, int64(addr))
	if n == 0 && err != nil {
		return "", fmt.Errorf("read proc mem at %#x: %w", addr, err)
	}
	buf = buf[:n]
	if i := indexByte(buf, 0); i >= 0 {
		return string(buf[:i]), nil
	}
	return "", fmt.Errorf("unterminated path (>= %d bytes)", max)
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// FdFromEnv reads a numeric fd from an environment variable (used to receive
// the listener fd when the launcher is a direct child via ExtraFiles).
func FdFromEnv(name string) (int, bool) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
