//go:build linux

package seccomp

import (
	"encoding/binary"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openCall describes where an open-family syscall keeps its pathname, dirfd,
// and flags in the trapped argument registers. Populated per-arch by
// decodeOpen (syscalls_<arch>.go).
type openCall struct {
	pathArg  int  // index of the pathname pointer
	dirfdArg int  // index of the dirfd, or -1 if cwd-relative (open/creat)
	flagsArg int  // index of the flags arg, or -1
	implicit bool // creat: flags are implicitly O_WRONLY|O_CREAT|O_TRUNC
	openHow  bool // openat2: flags live as a u64 at *args[flagsArg] (open_how.flags)
}

// writeIntent reports whether this open will (or may) modify the file, so the
// supervisor can mark the path local. Best-effort: a failed memory read for
// openat2 is treated as no write-intent (DIRTY tracking is a bonus, not a
// correctness requirement).
func (c openCall) writeIntent(args [6]uint64, pid int) bool {
	if c.implicit {
		return true
	}
	if c.flagsArg < 0 {
		return false
	}
	var flags uint64
	if c.openHow {
		v, err := readU64(pid, args[c.flagsArg])
		if err != nil {
			return false
		}
		flags = v
	} else {
		flags = args[c.flagsArg]
	}
	acc := flags & uint64(unix.O_ACCMODE)
	if acc == uint64(unix.O_WRONLY) || acc == uint64(unix.O_RDWR) {
		return true
	}
	return flags&uint64(unix.O_TRUNC) != 0 || flags&uint64(unix.O_CREAT) != 0
}

// readU64 reads a little-endian u64 from another process's memory.
func readU64(pid int, addr uint64) (uint64, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var b [8]byte
	if _, err := f.ReadAt(b[:], int64(addr)); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}
