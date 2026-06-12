//go:build linux && amd64

package seccomp

// open-family syscall numbers on x86_64 and where each keeps its path/dirfd/
// flags. Must match the set the launcher's BPF traps (shim/launcher.c).
const (
	sysOpen    = 2
	sysOpenat  = 257
	sysCreat   = 85
	sysOpenat2 = 437
)

func decodeOpen(nr int32) (openCall, bool) {
	switch nr {
	case sysOpen:
		return openCall{pathArg: 0, dirfdArg: -1, flagsArg: 1}, true
	case sysOpenat:
		return openCall{pathArg: 1, dirfdArg: 0, flagsArg: 2}, true
	case sysCreat:
		return openCall{pathArg: 0, dirfdArg: -1, flagsArg: -1, implicit: true}, true
	case sysOpenat2:
		return openCall{pathArg: 1, dirfdArg: 0, flagsArg: 2, openHow: true}, true
	}
	return openCall{}, false
}
