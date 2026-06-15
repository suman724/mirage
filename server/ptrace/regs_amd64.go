//go:build linux && amd64

package ptrace

import "encoding/binary"

// amd64 user_regs_struct layout (struct pt_regs), one uint64 per slot. We only
// need orig_rax (the syscall number at entry) and the arg registers. Offsets
// are slot-index * 8.
const (
	regOrigRax = 15 // syscall number as seen at entry
	regRdi     = 14 // arg0
	regRsi     = 13 // arg1
	regRdx     = 12 // arg2
	regR10     = 7  // arg3
	regR8      = 9  // arg4
	regR9      = 8  // arg5
)

// argSlots maps logical syscall-arg index → register slot index (System V ABI
// syscall convention: rdi, rsi, rdx, r10, r8, r9).
var argSlots = [6]int{regRdi, regRsi, regRdx, regR10, regR8, regR9}

// amd64 syscall numbers for the open + exec family.
func decodeSyscall(nr int) (openCall, bool) {
	switch nr {
	case 2: // open(path, flags, mode)
		return openCall{pathArg: 0, dirfdArg: -1}, true
	case 257: // openat(dirfd, path, flags, mode)
		return openCall{pathArg: 1, dirfdArg: 0}, true
	case 437: // openat2(dirfd, path, how, size)
		return openCall{pathArg: 1, dirfdArg: 0}, true
	case 85: // creat(path, mode)
		return openCall{pathArg: 0, dirfdArg: -1}, true
	case 59: // execve(path, argv, envp)
		return openCall{pathArg: 0, dirfdArg: -1, isExec: true}, true
	case 322: // execveat(dirfd, path, argv, envp, flags)
		return openCall{pathArg: 1, dirfdArg: 0, isExec: true}, true
	default:
		return openCall{}, false
	}
}

func sysNR(regs []byte) int {
	return int(slot(regs, regOrigRax))
}

func arg(regs []byte, i int) uint64 {
	if i < 0 || i >= len(argSlots) {
		return 0
	}
	return slot(regs, argSlots[i])
}

func slot(regs []byte, idx int) uint64 {
	off := idx * 8
	if off+8 > len(regs) {
		return 0
	}
	return binary.LittleEndian.Uint64(regs[off:])
}
