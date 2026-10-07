// Package sandbox runs a command inside an isolated sandbox backend and captures its output.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/jirib/golang-jirib-utils/execout"
	"github.com/jirib/golang-jirib-utils/logctx"
)

// NetworkMode defines the network isolation policy.
type NetworkMode int

const (
	// NetworkNone disables network access in the sandbox (default).
	NetworkNone NetworkMode = iota
	// NetworkShared shares the host network namespace.
	NetworkShared
)

// RootMode defines how RootFS is mounted at "/" inside the sandbox.
type RootMode int

const (
	// RootReadOnly mounts RootFS read-only at "/" (default).
	RootReadOnly RootMode = iota
	// RootWritable mounts RootFS read-write at "/".
	RootWritable
	// RootOverlay mounts RootFS read-only with a tmpfs upper layer.
	RootOverlay
)

// Mount represents a host path bind-mounted into the sandbox.
type Mount struct {
	Src string
	Dst string
	RO  bool
}

// ReadOnlyMount returns a read-only Mount from src to dst.
func ReadOnlyMount(src, dst string) Mount { return Mount{Src: src, Dst: dst, RO: true} }

// WritableMount returns a read-write Mount from src to dst.
func WritableMount(src, dst string) Mount { return Mount{Src: src, Dst: dst} }

// SandboxSpec specifies execution parameters for a sandboxed process.
type SandboxSpec struct {
	Command     []string
	RootFS      string
	Root        RootMode
	Mounts      []Mount
	PrivateTmp  bool
	Network     NetworkMode
	Env         []string
	ClearEnv    bool
	Cwd         string
	Timeout     time.Duration
	QuietOutput bool
}

// SandboxResult contains captured execution output.
type SandboxResult struct {
	Stdout    string
	Stderr    string
	Truncated bool
}

// SandboxBackend executes SandboxSpecs using a specific sandboxing technology.
type SandboxBackend interface {
	Name() string
	Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error)
}

// BwrapBackend implements SandboxBackend using bubblewrap.
type BwrapBackend struct{}

// NewBwrap returns a bubblewrap-backed SandboxBackend.
func NewBwrap() SandboxBackend { return BwrapBackend{} }

// Name returns "bwrap".
func (BwrapBackend) Name() string { return "bwrap" }

// Run executes spec using bubblewrap.
func (b BwrapBackend) Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return SandboxResult{}, fmt.Errorf("bwrap (bubblewrap) not found on PATH: %w", err)
	}
	args := bwrapArgs(spec)
	return execute(ctx, b.Name(), spec.Command, spec.Timeout, spec.QuietOutput, func(runCtx context.Context) *exec.Cmd {
		return exec.CommandContext(runCtx, "bwrap", args...)
	})
}

// Default is the default SandboxBackend used by Run.
var Default SandboxBackend = NewBwrap()

// Run executes spec using Default.
func Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error) {
	return Default.Run(ctx, spec)
}

// bwrapArgs translates a SandboxSpec into bubblewrap CLI arguments.
func bwrapArgs(spec SandboxSpec) []string {
	var args []string
	switch spec.Root {
	case RootOverlay:
		// Lower layer is RootFS (read-only); upper layer is tmpfs. --overlay-src must precede --tmp-overlay.
		args = append(args, "--overlay-src", spec.RootFS, "--tmp-overlay", "/")
	case RootWritable:
		args = append(args, "--bind", spec.RootFS, "/")
	default:
		args = append(args, "--ro-bind", spec.RootFS, "/")
	}
	args = append(args,
		"--dev", "/dev",
		"--proc", "/proc",
	)
	if spec.PrivateTmp {
		args = append(args, "--tmpfs", "/tmp")
	}
	for _, m := range spec.Mounts {
		flag := "--bind"
		if m.RO {
			flag = "--ro-bind"
		}
		args = append(args, flag, m.Src, m.Dst)
	}
	if spec.Network == NetworkNone {
		args = append(args, "--unshare-net")
	}
	args = append(args,
		"--unshare-pid",
		"--unshare-ipc",
		"--die-with-parent",
		"--new-session",
	)
	if spec.ClearEnv {
		args = append(args, "--clearenv")
	}
	for _, e := range spec.Env {
		k, v, _ := strings.Cut(e, "=")
		args = append(args, "--setenv", k, v)
	}
	if spec.Cwd != "" {
		args = append(args, "--chdir", spec.Cwd)
	}
	args = append(args, "--")
	args = append(args, spec.Command...)
	return args
}

// boundedBuffer captures up to cap bytes and discards further writes, marking truncated.
type boundedBuffer struct {
	cap       int
	buf       bytes.Buffer
	truncated bool
}

func (bb *boundedBuffer) Write(p []byte) (int, error) {
	if bb.truncated {
		return len(p), nil
	}
	n := len(p)
	if bb.buf.Len()+n > bb.cap {
		remaining := bb.cap - bb.buf.Len()
		if remaining > 0 {
			bb.buf.Write(p[:remaining])
		}
		bb.truncated = true
		return len(p), nil
	}
	bb.buf.Write(p)
	return len(p), nil
}

func (bb *boundedBuffer) String() string { return bb.buf.String() }

// debugLineWriter streams output lines to slog at LevelDebug as they arrive.
type debugLineWriter struct {
	logger  *slog.Logger
	cmd     string
	stream  string
	quiet   bool
	pending []byte
}

func (w *debugLineWriter) Write(p []byte) (int, error) {
	if w.quiet || !w.logger.Enabled(context.Background(), slog.LevelDebug) {
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexAny(w.pending, "\r\n")
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(w.pending[:i])); line != "" {
			w.logger.Debug("cmd output",
				"component", "executor",
				"event", "exec.output",
				"stream", w.stream,
				"cmd", w.cmd,
				"line", line,
			)
		}
		w.pending = w.pending[i+1:]
	}
	return len(p), nil
}

func (w *debugLineWriter) flush() {
	if len(w.pending) > 0 && !w.quiet && w.logger.Enabled(context.Background(), slog.LevelDebug) {
		if line := strings.TrimSpace(string(w.pending)); line != "" {
			w.logger.Debug("cmd output",
				"component", "executor",
				"event", "exec.output",
				"stream", w.stream,
				"cmd", w.cmd,
				"line", line,
			)
		}
		w.pending = nil
	}
}

func tail(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:]
}

// execute runs a command with timeout, output capture (capped at 10MB), and outcome logging.
func execute(
	ctx context.Context,
	name string,
	command []string,
	timeout time.Duration,
	quietOutput bool,
	newCmd func(context.Context) *exec.Cmd,
) (SandboxResult, error) {
	if len(command) == 0 {
		return SandboxResult{}, errors.New("sandbox: command must not be empty")
	}
	if timeout <= 0 {
		return SandboxResult{}, fmt.Errorf("sandbox: timeout must be positive; got %v", timeout)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	const outputCap = 10 * 1024 * 1024
	outBuf := &boundedBuffer{cap: outputCap}
	errBuf := &boundedBuffer{cap: outputCap}

	execID := logctx.NewID()
	runCtx = logctx.WithExecID(runCtx, execID)
	log := logctx.From(runCtx)

	cmd := newCmd(runCtx)
	outWriter := &debugLineWriter{logger: log, cmd: command[0], stream: "stdout", quiet: quietOutput}
	errWriter := &debugLineWriter{logger: log, cmd: command[0], stream: "stderr", quiet: quietOutput}
	cmd.Stdout = io.MultiWriter(outBuf, outWriter)
	cmd.Stderr = io.MultiWriter(errBuf, errWriter)

	log.Info("exec",
		"component", "executor",
		"event", "exec.started",
		"sandbox", name,
		"cmd", command[0],
		"args", strings.Join(command[1:], " "),
	)
	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	outWriter.flush()
	errWriter.flush()

	res := SandboxResult{
		Stdout:    outBuf.String(),
		Stderr:    errBuf.String(),
		Truncated: outBuf.truncated || errBuf.truncated,
	}

	outcome := execout.Classify(runCtx.Err(), runErr, cmd.ProcessState)
	attrs := outcome.Attrs(elapsed, timeout,
		"component", "executor",
		"event", "exec.completed",
		"cmd", command[0],
	)
	if runErr != nil && len(res.Stderr) > 0 {
		attrs = append(attrs, "stderr", tail(res.Stderr, 4096))
	}
	log.Info("exec done", attrs...)

	if runErr != nil {
		return res, outcome.Wrap("sandboxed command", timeout, runErr)
	}
	return res, nil
}
