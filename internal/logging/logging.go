// Package logging provides a thin wrapper around log/slog for consistent
// structured logging across the Varroa codebase.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// New creates a slog.Logger with the given level and output format.
// The format must be "json" or "text". The writer is where log output
// is written (typically os.Stderr). Call slog.SetDefault() with the
// returned logger if global default is desired.
func New(level slog.Level, format string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

// NewFromEnv creates a logger from environment variables:
//
//	VARROA_LOG_LEVEL  — debug, info, warn, error (default: info)
//	VARROA_LOG_FORMAT — json, text (default: text)
func NewFromEnv() *slog.Logger {
	return New(envLogLevel(), envLogFormat(), os.Stderr)
}

// FromContext returns the slog.Logger from the context, or the default
// logger if none is present. This is the preferred way to obtain a logger
// in downstream code — it always returns a valid logger.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}

// WithContext returns a new context with the given logger stored in it.
// Use FromContext(ctx) to retrieve it.
func WithContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// WithController returns a copy of the logger with the "controller"
// attribute set to the given namespace/name key.
func WithController(logger *slog.Logger, key string) *slog.Logger {
	return logger.With("controller", key)
}

type loggerKey struct{}

func envLogLevel() slog.Level {
	switch os.Getenv("VARROA_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envLogFormat() string {
	if os.Getenv("VARROA_LOG_FORMAT") == "json" {
		return "json"
	}
	return "text"
}

// ParseLevel parses a log level string.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	return l, l.UnmarshalText([]byte(s))
}
