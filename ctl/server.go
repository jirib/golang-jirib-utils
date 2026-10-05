package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

// SocketMode is the file mode for the control socket (owner read/write only).
const SocketMode fs.FileMode = 0o600

// ErrClosed is returned by Serve after Close has been called.
var ErrClosed = errors.New("ctl: server closed")

// Handler answers one control request. Returning an error generates a TypeError frame.
type Handler func(ctx context.Context, typ Type, payload []byte) (result []byte, err error)

// Server accepts control connections on a Unix domain socket.
type Server struct {
	// Path is the socket path to bind. If empty, derived via DefaultSocketPath(App).
	Path string
	// App is used to derive the default socket path when Path is empty.
	App string
	// Handler answers incoming requests.
	Handler Handler
	// Logger receives log events. If nil, slog.Default() is used.
	Logger *slog.Logger
	// ReadTimeout bounds frame delivery time from clients.
	ReadTimeout time.Duration

	ln        net.Listener
	bound     fs.FileInfo
	closed    chan struct{}
	closeOnce sync.Once
}

// Log returns Logger or slog.Default() if nil.
func (s *Server) Log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// SocketPath returns the socket path bound by Listen.
func (s *Server) SocketPath() string { return s.Path }

// Listen binds the Unix domain socket, creating the parent directory if needed
// and setting permissions to SocketMode (0600).
func (s *Server) Listen() error {
	if s.Handler == nil {
		return errors.New("ctl: Server.Handler is required")
	}
	if s.Path == "" {
		if s.App == "" {
			return errors.New("ctl: Server needs either Path or App")
		}
		p, err := DefaultSocketPath(s.App)
		if err != nil {
			return err
		}
		s.Path = p
	}
	path := s.Path

	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}

	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("ctl: %s exists and is not a socket; refusing to remove it", path)
		}
		live, dialErr := alive(path)
		if live {
			return fmt.Errorf("ctl: %s is already served by a live daemon; stop it first or point the daemon at a different path", path)
		}
		if dialErr != nil {
			// Inconclusive dial error (e.g. permission or seccomp); refuse to unlink.
			return fmt.Errorf("ctl: cannot tell whether %s is a stale socket: %w", path, dialErr)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("ctl: removing stale socket %s: %w", path, err)
		}
		s.Log().Debug("removed stale control socket", "path", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("ctl: checking control socket %s: %w", path, err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("ctl: listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, SocketMode); err != nil {
		ln.Close()
		os.Remove(path)
		return fmt.Errorf("ctl: setting control socket mode on %s: %w", path, err)
	}
	if fi, err := os.Lstat(path); err == nil {
		s.bound = fi
	}
	s.ln = ln
	s.closed = make(chan struct{})
	s.Log().Info("control socket listening", "path", path, "mode", "0600", "dir", filepath.Dir(path))
	return nil
}

var dialUnix = func(path string) (net.Conn, error) {
	return net.DialTimeout("unix", path, 2*time.Second)
}

// alive reports whether a process is actively listening on path.
// Returns false only on ECONNREFUSED; other dial errors are returned as inconclusive.
func alive(path string) (live bool, err error) {
	c, err := dialUnix(path)
	if err == nil {
		c.Close()
		return true, nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Serve accepts connections until the listener is closed or ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("ctl: Listen before Serve")
	}
	var wg sync.WaitGroup
	defer wg.Wait()

	stop := context.AfterFunc(ctx, func() {
		s.Close()
	})
	defer stop()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || isClosed(s.closed) {
				return nil
			}
			return fmt.Errorf("ctl: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// invoke calls Handler and recovers panics, converting them into error replies.
func (s *Server) invoke(ctx context.Context, typ Type, payload []byte) (result []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.Log().Error("control handler panicked",
				"type", typ.String(), "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("control command failed: %v", r)
			result = nil
		}
	}()
	return s.Handler(ctx, typ, payload)
}

func errorPayload(err error) []byte {
	b, mErr := json.Marshal(map[string]string{"error": err.Error()})
	if mErr != nil {
		return []byte(`{"error":"control request failed"}`)
	}
	return b
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	start := time.Now()
	if s.ReadTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))
	}
	log := s.Log().With("peer", conn.RemoteAddr())

	typ, payload, err := ReadMessage(conn)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			log.Debug("control connection closed before sending a request")
			return
		}
		log.Warn("control socket protocol violation", "error", err)
		if s.ReadTimeout > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(s.ReadTimeout))
		}
		_ = WriteMessage(conn, TypeError, []byte(`{"error":"protocol violation"}`))
		return
	}
	if typ.IsResponse() {
		log.Warn("control socket received a response frame instead of a request", "type", typ.String())
		return
	}
	log = log.With("type", typ.String())

	result, err := s.invoke(ctx, typ, payload)
	if err != nil {
		log.Warn("control request failed", "error", err, "duration", time.Since(start))
		if s.ReadTimeout > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(s.ReadTimeout))
		}
		_ = WriteMessage(conn, TypeError, errorPayload(err))
		return
	}
	if s.ReadTimeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(s.ReadTimeout))
	}
	if err := WriteMessage(conn, TypeReply, result); err != nil {
		log.Debug("control reply write failed", "error", err)
		return
	}
	log.Debug("control request served", "bytes", len(result), "duration", time.Since(start))
}

// Close stops accepting connections and unlinks the socket file.
// os.SameFile and ModTime are checked to avoid unlinking a socket bound by a successor process
// (even if the inode was recycled by the filesystem).
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.closed != nil {
			close(s.closed)
		}
		if s.ln != nil {
			err = s.ln.Close()
		}
		if s.Path != "" {
			if fi, statErr := os.Lstat(s.Path); statErr == nil && fi.Mode()&os.ModeSocket != 0 {
				if s.bound != nil && (!os.SameFile(fi, s.bound) || !fi.ModTime().Equal(s.bound.ModTime())) {
					s.Log().Info("leaving control socket in place: it is no longer the one this server bound", "path", s.Path)
					return
				}
				if rerr := os.Remove(s.Path); rerr != nil && err == nil {
					err = rerr
				}
			}
		}
	})
	return err
}
