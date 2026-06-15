//go:build linux && arm64

package ptrace

import "encoding/binary"

// arm64 user_pt_regs: regs[0..30] (x0..x30), then sp, pc, pstate. The syscall
// number is in x8; syscall args are x0..x5. Offsets are index * 8.
const regSyscallNo = 8 // x8 holds the syscall number

// arm64 syscall numbers for the open + exec family. There is no bare open(2)
// or creat(2) on arm64 — glibc routes them through openat.
func decodeSyscall(nr int) (openCall, bool) {
	switch nr {
	case 56: // openat(dirfd, path, flags, mode)
		return openCall{pathArg: 1, dirfdArg: 0}, true
	case 437: // openat2(dirfd, path, how, size)
		return openCall{pathArg: 1, dirfdArg: 0}, true
	case 221: // execve(path, argv, envp)
		return openCall{pathArg: 0, dirfdArg: -1, isExec: true}, true
	case 281: // execveat(dirfd, path, argv, envp, flags)
		return openCall{pathArg: 1, dirfdArg: 0, isExec: true}, true
	default:
		return openCall{}, false
	}
}

func sysNR(regs []byte) int {
	return int(slot(regs, regSyscallNo))
}

// On arm64 the logical arg index equals the register index (x0..x5).
func arg(regs []byte, i int) uint64 {
	if i < 0 || i > 5 {
		return 0
	}
	return slot(regs, i)
}

func slot(regs []byte, idx int) uint64 {
	off := idx * 8
	if off+8 > len(regs) {
		return 0
	}
	return binary.LittleEndian.Uint64(regs[off:])
}
