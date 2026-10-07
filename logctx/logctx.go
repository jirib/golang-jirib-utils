// Package logctx propagates request-scoped *slog.Logger instances and causal correlation IDs via context.Context.
package logctx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

type (
	loggerKey      struct{}
	requestIDKey   struct{}
	operationIDKey struct{}
	execIDKey      struct{}
)

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

// HasLogger reports whether ctx carries an explicit *slog.Logger.
func HasLogger(ctx context.Context) bool {
	if ctx != nil {
		if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
			return true
		}
	}
	return false
}

// NewID generates a random 6-character hex identifier for correlating execution logs.
func NewID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

type attrsKey struct{}

// WithRequestID stores requestID in ctx and enriches the contextual logger with the "request_id" attribute.
func WithRequestID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if id == "" || RequestID(ctx) == id {
		return ctx
	}
	ctx = context.WithValue(ctx, requestIDKey{}, id)
	return With(ctx, From(ctx).With("request_id", id))
}

// RequestID extracts the request identifier from ctx, or returns "" if none.
func RequestID(ctx context.Context) string {
	if ctx != nil {
		if id, ok := ctx.Value(requestIDKey{}).(string); ok {
			return id
		}
	}
	return ""
}

// WithOperationID stores opID in ctx and enriches the contextual logger with the "operation_id" attribute.
func WithOperationID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if id == "" || OperationID(ctx) == id {
		return ctx
	}
	ctx = context.WithValue(ctx, operationIDKey{}, id)
	return With(ctx, From(ctx).With("operation_id", id))
}

// OperationID extracts the operation identifier from ctx, or returns "" if none.
func OperationID(ctx context.Context) string {
	if ctx != nil {
		if id, ok := ctx.Value(operationIDKey{}).(string); ok {
			return id
		}
	}
	return ""
}

// WithExecID stores execID in ctx and enriches the contextual logger with the "exec_id" attribute.
func WithExecID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if id == "" || ExecID(ctx) == id {
		return ctx
	}
	ctx = context.WithValue(ctx, execIDKey{}, id)
	return With(ctx, From(ctx).With("exec_id", id))
}

// ExecID extracts the execution identifier from ctx, or returns "" if none.
func ExecID(ctx context.Context) string {
	if ctx != nil {
		if id, ok := ctx.Value(execIDKey{}).(string); ok {
			return id
		}
	}
	return ""
}

// WithAttrs returns a copy of ctx whose contextual logger includes the provided attributes.
// Duplicate attributes with identical values are omitted to keep log records clean.
func WithAttrs(ctx context.Context, attrs ...any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(attrs) == 0 {
		return ctx
	}

	existing, _ := ctx.Value(attrsKey{}).(map[string]any)
	newMap := make(map[string]any, len(existing)+len(attrs)/2)
	for k, v := range existing {
		newMap[k] = v
	}

	var toAdd []any
	for i := 0; i < len(attrs); i += 2 {
		if i+1 < len(attrs) {
			k, ok := attrs[i].(string)
			if ok {
				v := attrs[i+1]
				if oldVal, found := newMap[k]; found && oldVal == v {
					continue
				}
				newMap[k] = v
				toAdd = append(toAdd, k, v)
			} else {
				toAdd = append(toAdd, attrs[i], attrs[i+1])
			}
		} else {
			toAdd = append(toAdd, attrs[i])
		}
	}

	if len(toAdd) == 0 {
		return ctx
	}
	ctx = context.WithValue(ctx, attrsKey{}, newMap)
	return With(ctx, From(ctx).With(toAdd...))
}

// FromWithID returns the logger from ctx decorated with an "exec_id" attribute if id is non-empty.
func FromWithID(ctx context.Context, id string) *slog.Logger {
	l := From(ctx)
	if id == "" {
		return l
	}
	return l.With("exec_id", id)
}
