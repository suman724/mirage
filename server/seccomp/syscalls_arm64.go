//go:build linux && arm64

package seccomp

// open-family syscall numbers on arm64. arm64 has no legacy open(2)/creat(2) —
// glibc uses openat. Must match the launcher's BPF (shim/launcher.c).
const (
	sysOpenat  = 56
	sysOpenat2 = 437
)

func decodeOpen(nr int32) (openCall, bool) {
	switch nr {
	case sysOpenat:
		return openCall{pathArg: 1, dirfdArg: 0, flagsArg: 2}, true
	case sysOpenat2:
		return openCall{pathArg: 1, dirfdArg: 0, flagsArg: 2, openHow: true}, true
	}
	return openCall{}, false
}
