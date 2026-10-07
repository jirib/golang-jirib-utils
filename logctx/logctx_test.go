package logctx

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"
)

func TestFrom_FallsBackToDefaultWithoutWith(t *testing.T) {
	// The library-as-dependency case: a host that set its own slog default
	// expects to see output from these packages, so the fallback is the
	// default logger rather than a discard handler.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if got := From(context.Background()); got != slog.Default() {
		t.Errorf("From with no With = %p, want slog.Default() %p", got, slog.Default())
	}
	From(context.Background()).Info("from fallback")
	if !strings.Contains(buf.String(), "from fallback") {
		t.Error("fallback logger must actually write to slog.Default()'s handler")
	}
}

func TestFrom_NilContextIsSafe(t *testing.T) {
	// Callers must never have to nil-check before logging; a nil ctx
	// yielding the default logger keeps that true.
	if got := From(nil); got != slog.Default() {
		t.Errorf("From(nil) = %p, want slog.Default()", got)
	}
}

func TestWith_NilLoggerIsNoOp(t *testing.T) {
	// The no-logger-configured case must stay a working no-op rather than
	// storing a nil that From would have to defend against.
	ctx := context.Background()
	if got := With(ctx, nil); got != ctx {
		t.Error("With(ctx, nil) must return ctx unchanged")
	}
}

func TestWith_PropagatesToDerivedContexts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	ctx := With(context.Background(), logger)

	type key struct{}
	derived, cancel := context.WithCancel(ctx)
	defer cancel()
	if got := From(derived); got != logger {
		t.Error("logger must survive context.WithCancel (the timeout path in the exec code)")
	}

	if got := From(context.WithValue(ctx, key{}, 1)); got != logger {
		t.Error("logger must survive context.WithValue")
	}
}

func TestFromWithID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	FromWithID(With(context.Background(), logger), "a3f9c1").Debug("exec", "cmd", "zypper")
	line := buf.String()

	for _, want := range []string{"exec_id=a3f9c1", "cmd=zypper"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
}

func TestFromWithID_EmptyIDAddsNoAttribute(t *testing.T) {
	// A caller that doesn't want the id passes "" and must not get an
	// empty exec_id= polluting every line.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	FromWithID(With(context.Background(), logger), "").Debug("exec", "cmd", "zypper")

	if strings.Contains(buf.String(), "exec_id") {
		t.Errorf("empty id must not be logged, got %q", buf.String())
	}
}

func TestNewID(t *testing.T) {
	seen := make(map[string]bool, 512)
	for range 512 {
		id := NewID()
		if len(id) != 6 {
			t.Fatalf("NewID() = %q, want 6 hex chars", id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("NewID() = %q is not hex: %v", id, err)
		}
		seen[id] = true
	}
	// Not a uniqueness test in the strict sense — 512 draws from 16.7M
	// collide with probability ~0.8% — but a broken implementation (a
	// constant, say) would fail here immediately.
	if len(seen) < 500 {
		t.Errorf("only %d distinct ids out of 512 draws; expected near-uniqueness", len(seen))
	}
}

func TestWithRequestID(t *testing.T) {
	if got := RequestID(nil); got != "" {
		t.Errorf("RequestID(nil) = %q, want empty", got)
	}
	if got := RequestID(context.Background()); got != "" {
		t.Errorf("RequestID(empty ctx) = %q, want empty", got)
	}

	if HasLogger(nil) {
		t.Errorf("HasLogger(nil) = true, want false")
	}
	if HasLogger(context.Background()) {
		t.Errorf("HasLogger(empty ctx) = true, want false")
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := With(context.Background(), logger)

	if !HasLogger(ctx) {
		t.Errorf("HasLogger(ctx with logger) = false, want true")
	}

	// Empty ID leaves context and logger unmodified
	if got := WithRequestID(ctx, ""); RequestID(got) != "" {
		t.Errorf("WithRequestID with empty id should not set request_id")
	}

	ctx = WithRequestID(ctx, "req-8f31")
	if got := RequestID(ctx); got != "req-8f31" {
		t.Errorf("RequestID = %q, want req-8f31", got)
	}

	From(ctx).Info("request received")
	line := buf.String()
	if !strings.Contains(line, "request_id=req-8f31") {
		t.Errorf("log line missing request_id: %s", line)
	}
}

func TestWithOperationID(t *testing.T) {
	if got := OperationID(nil); got != "" {
		t.Errorf("OperationID(nil) = %q, want empty", got)
	}
	if got := OperationID(context.Background()); got != "" {
		t.Errorf("OperationID(empty ctx) = %q, want empty", got)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := With(context.Background(), logger)

	if got := WithOperationID(ctx, ""); OperationID(got) != "" {
		t.Errorf("WithOperationID with empty id should not set operation_id")
	}

	ctx = WithOperationID(ctx, "op-91ac")
	if got := OperationID(ctx); got != "op-91ac" {
		t.Errorf("OperationID = %q, want op-91ac", got)
	}

	From(ctx).Info("operation started")
	line := buf.String()
	if !strings.Contains(line, "operation_id=op-91ac") {
		t.Errorf("log line missing operation_id: %s", line)
	}
}

func TestWithExecID(t *testing.T) {
	if got := ExecID(nil); got != "" {
		t.Errorf("ExecID(nil) = %q, want empty", got)
	}
	if got := ExecID(context.Background()); got != "" {
		t.Errorf("ExecID(empty ctx) = %q, want empty", got)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := With(context.Background(), logger)

	if got := WithExecID(ctx, ""); ExecID(got) != "" {
		t.Errorf("WithExecID with empty id should not set exec_id")
	}

	ctx = WithExecID(ctx, "3144d2")
	if got := ExecID(ctx); got != "3144d2" {
		t.Errorf("ExecID = %q, want 3144d2", got)
	}

	From(ctx).Info("command run")
	line := buf.String()
	if !strings.Contains(line, "exec_id=3144d2") {
		t.Errorf("log line missing exec_id: %s", line)
	}
}

func TestWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := With(context.Background(), logger)

	// No attrs returns unchanged
	if got := WithAttrs(ctx); got != ctx {
		t.Errorf("WithAttrs without attributes should return same context")
	}

	ctx = WithAttrs(ctx, "target", "opensuse/tumbleweed", "package", "foo-1.2")
	From(ctx).Info("target ready")
	line := buf.String()

	for _, want := range []string{"target=opensuse/tumbleweed", "package=foo-1.2"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %s", want, line)
		}
	}
}

func TestCausalChain(t *testing.T) {
	var buf bytes.Buffer
	baseLogger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Step 1: Transport layer sets base logger and request_id
	reqCtx := With(context.Background(), baseLogger)
	reqCtx = WithRequestID(reqCtx, "8f31")

	// Step 2: Domain operation sets operation_id and domain attributes
	opCtx := WithOperationID(reqCtx, "op-91ac")
	opCtx = WithAttrs(opCtx, "target", "opensuse/tumbleweed", "package", "foo-1.2")

	// Step 3: Lower-level execution sets exec_id
	execCtx := WithExecID(opCtx, "3144d2")

	// Check ID retrieval at each stage
	if RequestID(execCtx) != "8f31" {
		t.Errorf("RequestID = %q, want 8f31", RequestID(execCtx))
	}
	if OperationID(execCtx) != "op-91ac" {
		t.Errorf("OperationID = %q, want op-91ac", OperationID(execCtx))
	}
	if ExecID(execCtx) != "3144d2" {
		t.Errorf("ExecID = %q, want 3144d2", ExecID(execCtx))
	}

	// Logging from execCtx must contain all layers
	From(execCtx).Info("command completed", "outcome", "success", "duration", "1.24s")
	line := buf.String()

	for _, want := range []string{
		"request_id=8f31",
		"operation_id=op-91ac",
		"target=opensuse/tumbleweed",
		"package=foo-1.2",
		"exec_id=3144d2",
		"outcome=success",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("causal log line missing %q:\n%s", want, line)
		}
	}
}

func TestWithAttrs_DeduplicatesAttributes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx := With(context.Background(), logger)
	ctx = WithRequestID(ctx, "req-1")
	ctx = WithRequestID(ctx, "req-1") // redundant call

	ctx = WithAttrs(ctx, "target", "opensuse/tumbleweed", "package", "hello")
	ctx = WithAttrs(ctx, "target", "opensuse/tumbleweed") // redundant call

	From(ctx).Info("test event")

	line := buf.String()
	// Count occurrences of "target=opensuse/tumbleweed"
	if strings.Count(line, "target=opensuse/tumbleweed") != 1 {
		t.Errorf("expected target to appear exactly once, got line: %s", line)
	}
	if strings.Count(line, "request_id=req-1") != 1 {
		t.Errorf("expected request_id to appear exactly once, got line: %s", line)
	}
}

