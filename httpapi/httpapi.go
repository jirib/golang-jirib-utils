// Package httpapi provides an HTTP API server for JSON command handlers with optional bearer authentication.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jirib/golang-jirib-utils/mcpserver/auth"
)

// maxBody bounds request payload size (1 MiB).
const maxBody = 1 << 20

// Handler executes a command from a raw JSON payload, returning a result or an error.
type Handler func(ctx context.Context, payload []byte) (any, error)

// Server is an HTTP server routing requests to command handlers.
type Server struct {
	Addr   string
	Token  string
	Realm  string
	Logger *slog.Logger

	mux *http.ServeMux
	srv *http.Server
}

// New constructs a Server with an empty route table.
func New(addr string, logger *slog.Logger) *Server {
	return &Server{Addr: addr, Logger: logger, mux: http.NewServeMux()}
}

// Handle registers handler h for the specified HTTP method and route pattern.
func (s *Server) Handle(method, pattern string, h Handler) {
	s.mux.HandleFunc(method+" "+pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.invoke(w, r, h)
	})
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) invoke(w http.ResponseWriter, r *http.Request, h Handler) {
	start := time.Now()
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		s.log().Warn("http request body read failed", "method", r.Method, "path", r.URL.Path, "error", err)
		s.writeError(w, err)
		return
	}
	if len(payload) > maxBody {
		s.log().Warn("http request body exceeds size limit", "method", r.Method, "path", r.URL.Path, "limit", maxBody)
		s.writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body exceeds limit"})
		return
	}

	result, err := h(r.Context(), payload)
	if err != nil {
		s.log().Warn("http request failed", "method", r.Method, "path", r.URL.Path, "error", err, "duration", time.Since(start))
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, result)
	s.log().Debug("http request served", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
}

// Handler returns the underlying http.Handler, applying BearerMiddleware if Token is configured.
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	if s.Token != "" {
		realm := s.Realm
		if realm == "" {
			realm = "api"
		}
		h = auth.BearerMiddleware(s.Token, realm, h)
	}
	return h
}

// Serve starts listening on Addr and serves requests until ctx is done, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	s.srv = &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("httpapi: listening on %s: %w", s.Addr, err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	}
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	s.writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	s.writeJSONStatus(w, http.StatusOK, v)
}

func (s *Server) writeJSONStatus(w http.ResponseWriter, status int, v any) {
	if v == nil {
		w.WriteHeader(status)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		s.log().Error("httpapi: encoding reply", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
