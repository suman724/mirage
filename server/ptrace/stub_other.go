//go:build !linux

// Package ptrace's interception is Linux-only (ptrace + seccomp RET_TRACE).
// This stub lets the module build on other platforms (e.g. macOS dev machines);
// the real implementation is in tracer_linux.go.
package ptrace

import (
	"errors"
	"log/slog"
)

// ErrUnsupported is returned by all operations on non-Linux platforms.
var ErrUnsupported = errors.New("ptrace: interception is only available on Linux")

// Materializer mirrors the Linux interface so callers compile cross-platform.
type Materializer interface {
	Root() string
	RelPath(abs string) (string, error)
	Ensure(rel string) error
}

// Stats mirrors the Linux type.
type Stats struct {
	Traps, Workspace, Errors uint64
}

// Tracer is a non-functional placeholder on non-Linux platforms.
type Tracer struct{}

// New returns ErrUnsupported off Linux.
func New(_ Materializer, _ *slog.Logger) (*Tracer, error) { return nil, ErrUnsupported }

// Serve returns ErrUnsupported off Linux.
func (t *Tracer) Serve(_ string) error { return ErrUnsupported }

// ExitCode returns -1 off Linux.
func (t *Tracer) ExitCode() int { return -1 }

// Stats returns a zero snapshot off Linux.
func (t *Tracer) Stats() Stats { return Stats{} }
