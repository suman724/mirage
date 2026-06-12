//go:build !linux

package main

import "fmt"
import "os"

func main() {
	fmt.Fprintln(os.Stderr, "mirage-seccomp-harness is Linux-only (seccomp user-notification)")
	os.Exit(2)
}
