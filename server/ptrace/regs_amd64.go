//go:build linux && amd64

package ptrace

import (
	"encoding/binary"
	"fmt"
)

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

// rax is register slot 10 — the syscall return value at exit.
const regRax = 10

// setReturn overwrites rax (the syscall return register) with val via the
// general-purpose regset. Used to inject -EIO at a neutralized syscall's exit.
func setReturn(pid int, val int64) error {
	regs, err := getRegs(pid)
	if err != nil {
		return err
	}
	if regRax*8+8 > len(regs) {
		return fmt.Errorf("ptrace: regs too small for rax")
	}
	binary.LittleEndian.PutUint64(regs[regRax*8:], uint64(val))
	return setRegset(pid, ntPrStatus, regs)
}

// skipSyscall neutralizes the pending syscall so the kernel does not execute it,
// by setting orig_rax to -1. On x86_64 the dispatched number lives in the GPR
// set, so this is enough; the caller then steps to the exit stop to set the
// return value.
func skipSyscall(pid int) error {
	regs, err := getRegs(pid)
	if err != nil {
		return err
	}
	if regOrigRax*8+8 > len(regs) {
		return fmt.Errorf("ptrace: regs too small for orig_rax")
	}
	binary.LittleEndian.PutUint64(regs[regOrigRax*8:], ^uint64(0)) // -1
	return setRegset(pid, ntPrStatus, regs)
}
