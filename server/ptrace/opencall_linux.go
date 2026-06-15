//go:build linux

package ptrace

// openCall says where an open/exec-family syscall keeps its pathname and dirfd
// in the trapped argument registers. Populated per-arch by decodeSyscall.
type openCall struct {
	pathArg  int // logical syscall-arg index of the pathname pointer
	dirfdArg int // logical arg index of the dirfd, or -1 if cwd-relative
	isExec   bool
}
