//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "mirage-ptrace-harness is Linux-only (ptrace + seccomp RET_TRACE)")
	os.Exit(2)
}
