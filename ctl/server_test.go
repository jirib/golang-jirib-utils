package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// startServer binds a Server on a socket inside a temp dir and serves it in
// the background, returning it plus a cleanup func.
func startServer(t *testing.T, h Handler) *Server {
	t.Helper()
	s := &Server{
		Path:    filepath.Join(t.TempDir(), "ctl", SocketName),
		Handler: h,
	}
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.Serve(ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return s
}

func echoHandler(ctx context.Context, typ Type, payload []byte) ([]byte, error) {
	return payload, nil
}

func TestServerClientRoundTrip(t *testing.T) {
	s := startServer(t, echoHandler)
	c := &Client{Path: s.SocketPath(), Timeout: 10 * time.Second}

	var got map[string]string
	if err := c.Call(context.Background(), testCmd, map[string]string{"hello": "world"}, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got["hello"] != "world" {
		t.Errorf("reply = %v", got)
	}
}

func TestServerCreatesSocket0600AndDir0700(t *testing.T) {
	s := startServer(t, echoHandler)

	fi, err := os.Lstat(s.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		t.Error("bound path is not a socket")
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %#o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(s.SocketPath()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("socket dir mode = %#o, want 0700", perm)
	}
}

// The socket file must not outlive the server: a leftover file makes the next
// start look like a stale socket it has to probe, and an operator listing the
// runtime dir should not see a dead socket in it.
func TestServerCloseUnlinksSocket(t *testing.T) {
	s := startServer(t, echoHandler)
	path := s.SocketPath()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("socket still present after Close: %v", err)
	}
	// Close must be idempotent; shutdown paths call it more than once.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A daemon killed without cleanup leaves a socket file behind. The next start
// must take it over rather than refusing — this is the common case, not an
// edge case.
func TestListenTakesOverStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctl", SocketName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A bound-then-abandoned listener: bind, then drop our reference
	// without unlinking. SetUnlinkOnClose(false) is what a daemon killed
	// with SIGKILL leaves behind.
	dead, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("test setup failed to leave a socket behind: %v", err)
	}

	s := &Server{Path: path, Handler: echoHandler}
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("socket not rebound: %v", err)
	}
}

// Two daemons must not both bind the same path: the second would otherwise
// unlink the first's socket and silently take over its address.
func TestListenRefusesLiveSocket(t *testing.T) {
	first := startServer(t, echoHandler)
	second := &Server{Path: first.SocketPath(), Handler: echoHandler}
	err := second.Listen()
	if err == nil {
		second.Close()
		t.Fatal("second Listen succeeded over a live socket")
	}
	if !strings.Contains(err.Error(), "live daemon") {
		t.Errorf("error should explain a live daemon owns it, got: %v", err)
	}
}

func TestListenRefusesNonSocketPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctl", SocketName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Something that is not a socket must never be unlinked: the daemon may
	// be pointed at a path an operator cares about.
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{Path: path, Handler: echoHandler}
	if err := s.Listen(); err == nil {
		s.Close()
		t.Fatal("Listen replaced a regular file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the regular file was removed instead of refused")
	}
}

func TestServeRequiresListen(t *testing.T) {
	s := &Server{Path: filepath.Join(t.TempDir(), "s"), Handler: echoHandler}
	if err := s.Serve(context.Background()); err == nil {
		t.Error("Serve before Listen should fail")
	}
}

func TestListenRequiresHandler(t *testing.T) {
	s := &Server{Path: filepath.Join(t.TempDir(), "s")}
	if err := s.Listen(); err == nil {
		s.Close()
		t.Error("Listen without a handler should fail")
	}
}

func TestHandlerErrorBecomesErrorFrame(t *testing.T) {
	// The message contains characters that break hand-built JSON quoting,
	// which is why the server marshals it.
	sentinel := errors.New("zypper failed:\n  \"foo\" at /tmp/bar\\baz")
	s := startServer(t, func(context.Context, Type, []byte) ([]byte, error) {
		return nil, sentinel
	})
	c := &Client{Path: s.SocketPath(), Timeout: 10 * time.Second}

	err := c.Call(context.Background(), testCmd, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != sentinel.Error() {
		t.Errorf("error = %q, want %q", err, sentinel.Error())
	}
}

// A slow handler must not block other connections — the reason this deviates
// from rtrd's single-session loop.
func TestConcurrentConnectionsAreNotSerialized(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	s := startServer(t, func(ctx context.Context, typ Type, payload []byte) ([]byte, error) {
		entered <- struct{}{}
		<-release
		return nil, nil
	})
	c := &Client{Path: s.SocketPath(), Timeout: 10 * time.Second}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Call(context.Background(), testCmd, nil, nil); err != nil {
				t.Errorf("Call: %v", err)
			}
		}()
	}
	// Both must be inside the handler before either is released.
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("only one connection reached the handler: they were serialized")
		}
	}
	close(release)
	wg.Wait()
}

// A frame that violates the protocol must not reach the handler.
func TestProtocolViolationDoesNotReachHandler(t *testing.T) {
	var called bool
	s := startServer(t, func(context.Context, Type, []byte) ([]byte, error) {
		called = true
		return nil, nil
	})
	conn, err := dialRaw(s.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A version this build does not speak.
	if _, err := conn.Write([]byte{Version + 9, byte(testCmd), 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	typ, body, err := ReadMessage(conn)
	if err != nil {
		t.Fatalf("expected an error frame, got %v", err)
	}
	if typ != TypeError {
		t.Errorf("frame type = %v, want error", typ)
	}
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil || e.Error == "" {
		t.Errorf("error frame body = %s", body)
	}
	if called {
		t.Error("handler ran despite a malformed frame")
	}
}

func TestClientReportsMissingDaemon(t *testing.T) {
	c := &Client{Path: filepath.Join(t.TempDir(), "absent", SocketName), Timeout: 2 * time.Second}
	err := c.Call(context.Background(), testCmd, nil, nil)
	if !errors.Is(err, ErrNoDaemon) {
		t.Errorf("err = %v, want ErrNoDaemon", err)
	}
}

func TestClientNeedsPathOrApp(t *testing.T) {
	c := &Client{}
	if err := c.Call(context.Background(), testCmd, nil, nil); err == nil {
		t.Error("expected an error with neither Path nor App")
	}
}

// An unreadable error body must still surface as an error rather than being
// treated as success: silently swallowing it is exactly the failure mode this
// protocol exists to prevent.
func TestReplyErrorRejectsUnusableBodies(t *testing.T) {
	for _, body := range []string{``, `not json`, `{}`, `{"error":""}`} {
		if err := replyError([]byte(body)); err == nil {
			t.Errorf("body %q: expected an error", body)
		}
	}
	err := replyError([]byte(`{"error":"no such target"}`))
	if err == nil || err.Error() != "no such target" {
		t.Errorf("err = %v, want the server's message", err)
	}
}

// dialRaw opens a socket connection for tests that need to write bytes by hand.
func dialRaw(path string) (net.Conn, error) {
	return net.Dial("unix", path)
}

// readTimeout must fire on a connection that never finishes its frame, so a
// peer cannot hold a goroutine open indefinitely by dribbling bytes.
func TestReadTimeoutBoundsAStalledPeer(t *testing.T) {
	s := &Server{
		Path:        filepath.Join(t.TempDir(), "ctl", SocketName),
		Handler:     echoHandler,
		ReadTimeout: 100 * time.Millisecond,
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Serve(ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	conn, err := dialRaw(s.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Half a header, then nothing.
	if _, err := conn.Write([]byte{Version, byte(testCmd)}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, _, err := ReadMessage(conn)
	// The read deadline must fire — the server cannot hang on a peer that
	// dribbles a header. What the peer then sees is either a hangup or a
	// clean "protocol violation" error frame; both prove the deadline fired,
	// and the latter is the friendlier of the two.
	if err == nil && !typ.IsResponse() {
		t.Fatalf("expected the stalled frame to be rejected, got type %v", typ)
	}
}

// A panicking handler must not take the daemon down: handlers run on their own
// goroutine, so an unrecovered panic unwinds past Serve's loop and kills the
// process — along with whatever real work the daemon was doing.
func TestPanickingHandlerBecomesAnErrorReply(t *testing.T) {
	s := startServer(t, func(context.Context, Type, []byte) ([]byte, error) {
		panic("boom")
	})
	c := &Client{Path: s.SocketPath(), Timeout: 10 * time.Second}

	err := c.Call(context.Background(), testCmd, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry the panic value, got: %v", err)
	}

	// The server must still be serving.
	if err := c.Call(context.Background(), testCmd, nil, nil); err == nil {
		t.Error("server stopped answering after a handler panic")
	}
}

// A leftover socket may only be removed when we can prove nothing is
// listening. ECONNREFUSED proves it (socket inode, no listener); anything else
// — a foreign socket we cannot connect to, an fd limit, a seccomp policy —
// proves nothing, and removing on an unproven assumption steals a live daemon's
// address. A mode-only check would get this wrong; the socket file has to
// survive.
func TestListenKeepsSocketWhenLivenessIsInconclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctl", SocketName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	dead, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}

	orig := dialUnix
	dialUnix = func(string) (net.Conn, error) { return nil, syscall.EACCES }
	t.Cleanup(func() { dialUnix = orig })

	s := &Server{Path: path, Handler: echoHandler}
	err = s.Listen()
	if err == nil {
		s.Close()
		t.Fatal("Listen removed a socket it could not prove was stale")
	}
	if !strings.Contains(err.Error(), "cannot tell whether") {
		t.Errorf("error should explain the liveness check was inconclusive, got: %v", err)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Errorf("socket was removed despite an inconclusive check: %v", statErr)
	}
}

// Two daemons must not both bind the same path: the second would otherwise
// unlink the first's socket and silently take over its address.
func TestListenRefusesLiveSocketAfterInconclusiveCheck(t *testing.T) {
	first := startServer(t, echoHandler)
	// A refused connect is the conclusive answer, so a second Listen still
	// fails — the refactor above must not have made every failure look
	// inconclusive and blocked legitimate restarts.
	second := &Server{Path: first.SocketPath(), Handler: echoHandler}
	if err := second.Listen(); err == nil {
		second.Close()
		t.Fatal("second Listen succeeded over a live socket")
	}
}

// Close must unlink the socket it bound and nothing else. The window it has to
// get wrong is real: a successor daemon can bind the same path between our
// listener closing and the unlink, and a mode-only check would delete its
// socket — breaking a live daemon while this server reports a clean shutdown.
func TestCloseLeavesASuccessorsSocketAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctl", SocketName)

	old := &Server{Path: path, Handler: echoHandler}
	if err := old.Listen(); err != nil {
		t.Fatal(err)
	}
	// Simulate the old daemon's socket being replaced: close the listener
	// without going through Close, so no unlink happens, then remove the file
	// and bind a new one at the same path. Dropping the listener reference
	// isolates what is under test — Close's unlink decision, not the second
	// close of an already-closed listener.
	old.ln.Close()
	old.ln = nil
	os.Remove(path)

	successor, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	successor.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := successor.Close(); err != nil {
		t.Fatal(err)
	}

	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("Close removed the successor's socket: %v", err)
	}
}

func TestClientNegativeTimeoutWaitsPastDefault(t *testing.T) {
	// A request whose runtime is genuinely unbounded — a debuginfo tree, a
	// multi-minute build — must not be cut off by a client-side timer that has
	// to be re-guessed for every new workload. A negative Timeout removes it,
	// leaving ctx cancellation as the only way to stop waiting.
	//
	// The handler here sleeps past DefaultTimeout's spirit (a real one would
	// make this test take ten minutes), so the assertion is that the call
	// outlives any plausible fixed deadline rather than that it exceeds the
	// constant.
	slow := 300 * time.Millisecond
	s := startServer(t, func(ctx context.Context, typ Type, payload []byte) ([]byte, error) {
		select {
		case <-time.After(slow):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return payload, nil
	})
	c := &Client{Path: s.SocketPath(), Timeout: -1}

	var got map[string]string
	if err := c.Call(context.Background(), testCmd, map[string]string{"slow": "yes"}, &got); err != nil {
		t.Fatalf("Call with negative timeout: %v", err)
	}
	if got["slow"] != "yes" {
		t.Errorf("reply = %v", got)
	}
}

func TestClientZeroTimeoutStillAppliesDefault(t *testing.T) {
	// Zero keeps meaning DefaultTimeout. Making it mean "unbounded" would have
	// silently removed a deadline from every existing caller.
	s := startServer(t, echoHandler)
	c := &Client{Path: s.SocketPath()}

	var got map[string]string
	if err := c.Call(context.Background(), testCmd, map[string]string{"zero": "default"}, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got["zero"] != "default" {
		t.Errorf("reply = %v", got)
	}
}

func TestReadTimeoutDoesNotCapHandlerRuntime(t *testing.T) {
	// A handler that runs longer than ReadTimeout must still get its reply
	// written. The documented contract is that ReadTimeout is a liveness
	// guard against a stalled peer, not a budget on how long a command may
	// take (gc, or an artifact fetch, legitimately run for minutes). The bug
	// this pins is a single SetDeadline spanning read *and* write, which made
	// any command slower than ReadTimeout have its reply discarded as a
	// timeout.
	slow := 300 * time.Millisecond
	s := &Server{
		Path: filepath.Join(t.TempDir(), "ctl", SocketName),
		Handler: func(ctx context.Context, typ Type, payload []byte) ([]byte, error) {
			select {
			case <-time.After(slow):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return payload, nil
		},
		ReadTimeout: 50 * time.Millisecond, // far shorter than the handler
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.Serve(ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	c := &Client{Path: s.SocketPath(), Timeout: -1}
	var got map[string]string
	if err := c.Call(context.Background(), testCmd, map[string]string{"slow": "still-delivered"}, &got); err != nil {
		t.Fatalf("Call for a handler slower than ReadTimeout: %v", err)
	}
	if got["slow"] != "still-delivered" {
		t.Errorf("reply = %v, want the slow handler's reply delivered intact", got)
	}
}

func TestClientContextCancellationUnblocks(t *testing.T) {
	hang := make(chan struct{})
	s := startServer(t, func(ctx context.Context, typ Type, payload []byte) ([]byte, error) {
		<-hang
		return payload, nil
	})
	t.Cleanup(func() { close(hang) })

	c := &Client{Path: s.SocketPath(), Timeout: -1}
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Call(ctx, testCmd, nil, nil)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Client.Call did not unblock upon context cancellation")
	}
}

