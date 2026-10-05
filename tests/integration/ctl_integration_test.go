package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jirib/golang-jirib-utils/ctl"
)

// TestCTL_ClientServerEchoAndErrors exercises ctl.Server and ctl.Client over a real Unix domain socket.
func TestCTL_ClientServerEchoAndErrors(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "test.sock")

	const (
		typeEcho ctl.Type = 1
		typeFail ctl.Type = 2
	)

	handler := func(ctx context.Context, typ ctl.Type, payload []byte) ([]byte, error) {
		switch typ {
		case typeEcho:
			return append([]byte("echo:"), payload...), nil
		case typeFail:
			return nil, errors.New("simulated server handler failure")
		default:
			return nil, fmt.Errorf("unexpected command type: %d", typ)
		}
	}

	srv := &ctl.Server{
		Path:    sockPath,
		Handler: handler,
	}

	if err := srv.Listen(); err != nil {
		t.Fatalf("srv.Listen failed: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(); err != nil && !errors.Is(err, ctl.ErrClosed) {
			t.Errorf("srv.Serve failed: %v", err)
		}
	}()

	client := &ctl.Client{
		Path:    sockPath,
		Timeout: 2 * time.Second,
	}

	t.Run("EchoSuccess", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		reply, err := client.CallRaw(ctx, typeEcho, "hello-ctl")
		if err != nil {
			t.Fatalf("client.CallRaw echo failed: %v", err)
		}
		if got, want := string(reply), "echo:\"hello-ctl\""; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("HandlerErrorPropagation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := client.CallRaw(ctx, typeFail, "fail-request")
		if err == nil {
			t.Fatalf("expected handler error, got nil")
		}
		if !strings.Contains(err.Error(), "simulated server handler failure") {
			t.Errorf("expected error to contain simulated failure message, got: %v", err)
		}
	})

	// Close server and wait for Serve loop to finish
	srv.Close()
	wg.Wait()

	t.Run("DialAfterShutdown", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		_, err := client.CallRaw(ctx, typeEcho, "after-close")
		if err == nil {
			t.Fatalf("expected call after shutdown to fail, got nil")
		}
	})
}

// TestCTL_ClientDeadlineExceeded verifies client-side timeout enforcement.
func TestCTL_ClientDeadlineExceeded(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "slow.sock")

	srv := &ctl.Server{
		Path: sockPath,
		Handler: func(ctx context.Context, typ ctl.Type, payload []byte) ([]byte, error) {
			// Sleep longer than client timeout
			time.Sleep(300 * time.Millisecond)
			return []byte("slow reply"), nil
		},
	}

	if err := srv.Listen(); err != nil {
		t.Fatalf("srv.Listen failed: %v", err)
	}
	defer srv.Close()

	go func() { _ = srv.Serve() }()

	client := &ctl.Client{
		Path:    sockPath,
		Timeout: 50 * time.Millisecond,
	}

	ctx := context.Background()
	_, err := client.CallRaw(ctx, 1, "ping")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && !netErr.Timeout() && !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("got error: %v", err)
	}
}

// TestCTL_StaleSocketCleanup proves that a stale socket file is automatically removed.
func TestCTL_StaleSocketCleanup(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "stale.sock")

	// Create a dummy socket listener then abruptly close the listener without unlinking
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	_ = ln.Close()

	if _, err := os.Lstat(sockPath); err != nil {
		t.Fatalf("expected stale socket to exist on disk: %v", err)
	}

	srv := &ctl.Server{
		Path: sockPath,
		Handler: func(ctx context.Context, typ ctl.Type, payload []byte) ([]byte, error) {
			return []byte("ok"), nil
		},
	}

	if err := srv.Listen(); err != nil {
		t.Fatalf("srv.Listen should have unlinked stale socket and succeeded, got: %v", err)
	}
	defer srv.Close()
}
