// Package logctx propagates request-scoped *slog.Logger instances via context.Context.
package logctx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

type loggerKey struct{}

// With returns a copy of ctx carrying logger. If logger is nil, ctx is returned unchanged.
func With(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerKey{}, logger)
}

// From extracts the logger from ctx, falling back to slog.Default() if none is found or if ctx is nil.
func From(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.Default()
}

// NewID generates a random 6-character hex identifier for correlating execution logs.
func NewID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// FromWithID returns the logger from ctx decorated with an "exec_id" attribute if id is non-empty.
func FromWithID(ctx context.Context, id string) *slog.Logger {
	l := From(ctx)
	if id == "" {
		return l
	}
	return l.With("exec_id", id)
}
