// Package logging builds the application's structured logger.
//
// We use log/slog from the standard library (Go 1.21+). It is the Go equivalent of
// pino/winston: key-value structured records, JSON output in production, and a
// handler you can swap out. slog.Logger is safe for concurrent use, so a single
// instance is shared by every goroutine.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON or text logger at the requested level.
func New(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

// NewWithService returns a logger that stamps every record with service=<name>,
// which is what you filter on in CloudWatch Logs Insights once api and worker
// share a log group.
func NewWithService(level, format, service string) *slog.Logger {
	return New(level, format).With(slog.String("service", service))
}

func parseLevel(s string) slog.Level {
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
