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
