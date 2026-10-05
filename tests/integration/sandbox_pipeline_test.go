package integration

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jirib/golang-jirib-utils/execout"
	"github.com/jirib/golang-jirib-utils/logctx"
	"github.com/jirib/golang-jirib-utils/sandbox"
)

// requireBwrap skips if bubblewrap is not installed.
func requireBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap (bubblewrap) not found on PATH; skipping sandbox integration test")
	}
}

// TestSandbox_PipelineIntegration tests the combined execution of sandbox, execout classification,
// and logctx context logger correlation.
func TestSandbox_PipelineIntegration(t *testing.T) {
	requireBwrap(t)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logctx.With(context.Background(), logger)

	topdir := t.TempDir()
	topdir, err := filepath.Abs(topdir)
	if err != nil {
		t.Fatal(err)
	}

	spec := sandbox.SandboxSpec{
		Command:    []string{"sh", "-c", "echo sandbox-start && echo sandbox-payload && exit 0"},
		RootFS:     "/",
		Root:       sandbox.RootReadOnly,
		Mounts:     []sandbox.Mount{sandbox.WritableMount(topdir, topdir)},
		PrivateTmp: true,
		Network:    sandbox.NetworkNone,
		Env:        []string{"HOME=" + topdir},
		Cwd:        topdir,
		Timeout:    10 * time.Second,
	}

	backend := sandbox.NewBwrap()
	res, runErr := backend.Run(ctx, spec)
	if runErr != nil {
		t.Fatalf("backend.Run failed: %v", runErr)
	}

	if !strings.Contains(res.Stdout, "sandbox-payload") {
		t.Fatalf("stdout missing expected payload: %q", res.Stdout)
	}

	// Verify log correlation
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "exec_id=") {
		t.Errorf("log output missing exec_id correlation attribute:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, `msg="exec"`) {
		t.Errorf("log output missing exec start message:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, `msg="exec done"`) {
		t.Errorf("log output missing exec done message:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, `outcome=ok`) {
		t.Errorf("log output missing outcome=ok attribute:\n%s", logOutput)
	}
}

// TestSandbox_PipelineErrorClassification ensures non-zero exits in a sandbox are correctly
// classified as OutcomeExit by execout and wrapped appropriately.
func TestSandbox_PipelineErrorClassification(t *testing.T) {
	requireBwrap(t)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logctx.With(context.Background(), logger)

	topdir := t.TempDir()
	topdir, err := filepath.Abs(topdir)
	if err != nil {
		t.Fatal(err)
	}

	spec := sandbox.SandboxSpec{
		Command:    []string{"sh", "-c", "exit 42"},
		RootFS:     "/",
		Root:       sandbox.RootReadOnly,
		Mounts:     []sandbox.Mount{sandbox.WritableMount(topdir, topdir)},
		PrivateTmp: true,
		Network:    sandbox.NetworkNone,
		Cwd:        topdir,
		Timeout:    5 * time.Second,
	}

	backend := sandbox.NewBwrap()
	_, runErr := backend.Run(ctx, spec)
	if runErr == nil {
		t.Fatal("expected error from exit 42, got nil")
	}

	if !strings.Contains(runErr.Error(), "sandboxed command") {
		t.Errorf("expected wrapped error to contain 'sandboxed command', got: %v", runErr)
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "outcome=exit") {
		t.Errorf("expected log output to record outcome=exit:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "rc=42") {
		t.Errorf("expected log output to record rc=42:\n%s", logOutput)
	}

	// Also verify execout.Classify standalone
	res := execout.Classify(nil, runErr, nil)
	if res.Outcome != execout.OutcomeExit {
		t.Errorf("expected execout.Classify to return OutcomeExit, got %v", res.Outcome)
	}
}
