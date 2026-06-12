//go:build !linux

// Package seccomp's syscall-level interception is Linux-only (seccomp
// user-notification). This stub lets the module build on other platforms
// (e.g. macOS dev machines); the real implementation is in seccomp_linux.go.
package seccomp

import (
	"errors"
	"log/slog"
)

// ErrUnsupported is returned by all operations on non-Linux platforms.
var ErrUnsupported = errors.New("seccomp: user-notification interception is only available on Linux")

// Materializer mirrors the Linux interface so callers compile cross-platform.
type Materializer interface {
	Root() string
	RelPath(abs string) (string, error)
	Ensure(rel string) error
	Dirty(rel string) error
}

// Stats mirrors the Linux type.
type Stats struct {
	Traps, Workspace, Materialized, FastPath, Errors uint64
}

// Supervisor is a non-functional placeholder on non-Linux platforms.
type Supervisor struct{}

// New returns ErrUnsupported off Linux.
func New(_ Materializer, _ *slog.Logger) (*Supervisor, error) { return nil, ErrUnsupported }

// Serve returns ErrUnsupported off Linux.
func (s *Supervisor) Serve(_, _ int) error { return ErrUnsupported }

// Stop is a no-op off Linux.
func (s *Supervisor) Stop() {}

// Stats returns a zero snapshot off Linux.
func (s *Supervisor) Stats() Stats { return Stats{} }

// FdFromEnv is unsupported off Linux.
func FdFromEnv(_ string) (int, bool) { return 0, false }
