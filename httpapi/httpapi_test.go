package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer() *Server {
	return New("", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// do performs a request against s's handler tree.
func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleRoutesAndReturnsJSONResult(t *testing.T) {
	s := newTestServer()
	s.Handle(http.MethodPost, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	})

	rec := do(t, s, http.MethodPost, "/things", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"ok":"yes"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestHandleAllowsSamePathWithDifferentMethods(t *testing.T) {
	s := newTestServer()
	s.Handle(http.MethodGet, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return map[string]string{"method": "get"}, nil
	})
	s.Handle(http.MethodPost, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return map[string]string{"method": "post"}, nil
	})

	get := do(t, s, http.MethodGet, "/things", "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", get.Code)
	}
	if got := strings.TrimSpace(get.Body.String()); got != `{"method":"get"}` {
		t.Fatalf("GET body = %q", got)
	}

	post := do(t, s, http.MethodPost, "/things", "")
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", post.Code)
	}
	if got := strings.TrimSpace(post.Body.String()); got != `{"method":"post"}` {
		t.Fatalf("POST body = %q", got)
	}
}

func TestHandleRejectsWrongMethod(t *testing.T) {
	s := newTestServer()
	s.Handle(http.MethodPost, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return nil, nil
	})

	if rec := do(t, s, http.MethodGet, "/things", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandlerErrorRendersErrorBody(t *testing.T) {
	s := newTestServer()
	s.Handle(http.MethodPost, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return nil, context.Canceled
	})

	rec := do(t, s, http.MethodPost, "/things", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"context canceled"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestNilResultProducesEmptyBody(t *testing.T) {
	s := newTestServer()
	s.Handle(http.MethodPost, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return nil, nil
	})

	rec := do(t, s, http.MethodPost, "/things", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

func TestHandlerReceivesBody(t *testing.T) {
	s := newTestServer()
	s.Handle(http.MethodPost, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return map[string]string{"got": string(payload)}, nil
	})

	rec := do(t, s, http.MethodPost, "/things", `{"a":1}`)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"got":"{\"a\":1}"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestInsecureByDefault(t *testing.T) {
	s := newTestServer()
	s.Handle(http.MethodGet, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return "ok", nil
	})

	// No Authorization header, no token configured: must succeed.
	if rec := do(t, s, http.MethodGet, "/things", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (insecure by default)", rec.Code)
	}
}

func TestTokenEnforcesAuth(t *testing.T) {
	s := newTestServer()
	s.Token = "secret"
	s.Handle(http.MethodGet, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return "ok", nil
	})

	if rec := do(t, s, http.MethodGet, "/things", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without token", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/things", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with token", rec.Code)
	}
}

func TestPayloadExceedingMaxBodyReturns413(t *testing.T) {
	s := newTestServer()
	s.Handle(http.MethodPost, "/things", func(ctx context.Context, payload []byte) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	})

	oversized := strings.Repeat("x", maxBody+10)
	rec := do(t, s, http.MethodPost, "/things", oversized)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(rec.Body.String(), "request body exceeds limit") {
		t.Fatalf("body = %q, want request body exceeds limit", rec.Body.String())
	}
}
