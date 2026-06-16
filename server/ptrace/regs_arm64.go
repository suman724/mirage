//go:build linux && arm64

package ptrace

import (
	"encoding/binary"
	"fmt"
)

// NT_ARM_SYSTEM_CALL: the regset used to change the dispatched syscall number on
// arm64 (writing x8 via the GPR set does NOT change dispatch here, unlike x86).
const ntArmSyscall = 0x404

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

// setReturn overwrites x0 (the syscall return register) with val via the
// general-purpose regset. Used to inject -EIO at a neutralized syscall's exit.
func setReturn(pid int, val int64) error {
	regs, err := getRegs(pid)
	if err != nil {
		return err
	}
	if len(regs) < 8 {
		return fmt.Errorf("ptrace: regs too small for x0")
	}
	binary.LittleEndian.PutUint64(regs[0:], uint64(val)) // x0 = regs[0]
	return setRegset(pid, ntPrStatus, regs)
}

// skipSyscall neutralizes the pending syscall by setting the dispatched number
// to -1 via NT_ARM_SYSTEM_CALL (arm64's dedicated mechanism — the GPR set can't
// do it). The caller then steps to the exit stop to set the return value.
func skipSyscall(pid int) error {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, ^uint32(0)) // (int)-1
	return setRegset(pid, ntArmSyscall, b)
}
