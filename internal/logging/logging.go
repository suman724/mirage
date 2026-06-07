// Package logging centralizes structured-logging setup so the client, server,
// and libraries emit consistent, leveled, machine-parseable logs via log/slog.
//
// Convention: binaries call Setup once and pass the returned *slog.Logger into
// the components they construct. Libraries accept a *slog.Logger and treat nil
// as slog.Default(), so they never panic on a missing logger and stay testable.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup builds a logger writing to stderr at the given level ("debug", "info",
// "warn", "error") in the given format ("text" or "json"). It also installs the
// logger as slog.Default so library code that falls back to the default stays
// consistent with the binary's configuration.
func Setup(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: ParseLevel(level)}
	var h slog.Handler
	switch strings.ToLower(format) {
	case "json":
		h = slog.NewJSONHandler(os.Stderr, opts)
	default:
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger
}

// ParseLevel maps a level string to slog.Level, defaulting to Info for unknown
// or empty input.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// OrDefault returns logger, or slog.Default() if logger is nil. Library
// constructors use this so callers may pass nil.
func OrDefault(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}
