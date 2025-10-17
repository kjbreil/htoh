// Package logger provides structured logging functionality using Go's log/slog package.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

const (
	// DefaultLevel is the default logging level used when no level is specified.
	DefaultLevel = slog.LevelInfo

	// LevelDisabled is a log level higher than Error used to effectively disable logging.
	// Set to 12, which is higher than LevelError (8).
	LevelDisabled = slog.Level(12)
)

// NewLogger creates a new slog.Logger instance with JSON output.
// It configures a JSON handler that writes to the provided output with the specified log level.
func NewLogger(level slog.Level, output io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		AddSource:   false,
		Level:       level,
		ReplaceAttr: nil,
	}
	handler := slog.NewJSONHandler(output, opts)
	return slog.New(handler)
}

// ParseLevel parses a string representation of a log level into a slog.Level.
// It accepts the following case-insensitive values: "debug", "info", "warn", "error".
// Returns an error if the provided string does not match any known log level.
func ParseLevel(s string) (slog.Level, error) {
	normalized := strings.ToLower(strings.TrimSpace(s))
	switch normalized {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level: %q (must be debug, info, warn, or error)", s)
	}
}
