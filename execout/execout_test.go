package execout

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// run executes a command and classifies it exactly as the callers do: the
// context error of the run context, the exec error, and the process state.
func run(t *testing.T, timeout time.Duration, name string, args ...string) (Result, error) {
	t.Helper()
	runCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, name, args...)
	runErr := cmd.Run()
	return Classify(runCtx.Err(), runErr, cmd.ProcessState), runErr
}

// A command that exits 0 is the only outcome with no error at all.
func TestClassify_OK(t *testing.T) {
	res, err := run(t, 5*time.Second, "/bin/true")
	if err != nil {
		t.Fatalf("want no error, got %v", err)
	}
	if res.Outcome != OutcomeOK {
		t.Errorf("outcome = %q, want %q", res.Outcome, OutcomeOK)
	}
	if res.RC != 0 {
		t.Errorf("rc = %d, want 0", res.RC)
	}
	if res.Signal != "" {
		t.Errorf("signal = %q, want empty for a clean exit", res.Signal)
	}
	if res.PID == 0 {
		t.Error("pid = 0, want the child's pid")
	}
}

// A non-zero exit is a command failure, not a timeout, even though it arrives
// as a non-nil exec error just like a timeout does.
func TestClassify_Exit(t *testing.T) {
	res, err := run(t, 5*time.Second, "/bin/false")
	if err == nil {
		t.Fatal("want an error for a non-zero exit")
	}
	if res.Outcome != OutcomeExit {
		t.Errorf("outcome = %q, want %q", res.Outcome, OutcomeExit)
	}
	if res.RC != 1 {
		t.Errorf("rc = %d, want 1", res.RC)
	}
	if res.Signal != "" {
		t.Errorf("signal = %q, want empty: the process exited, it was not signalled", res.Signal)
	}
}

// This is the regression that motivated the package. exec.CommandContext
// answers an expired deadline with SIGKILL, so the exec error is "signal:
// killed" and is non-nil — structurally identical to a command that died by
// signal. Classifying the exec error before the context error therefore
// reported every timeout as a plain failure, and the "timed out" error string
// was unreachable.
func TestClassify_TimeoutIsNotAFailure(t *testing.T) {
	res, err := run(t, 50*time.Millisecond, "/bin/sleep", "30")
	if err == nil {
		t.Fatal("want an error when the deadline expires")
	}
	if res.Outcome != OutcomeTimeout {
		t.Fatalf("outcome = %q, want %q (a deadline must not read as a failure)", res.Outcome, OutcomeTimeout)
	}
	if res.Signal != "killed" {
		t.Errorf("signal = %q, want %q", res.Signal, "killed")
	}
	// SIGKILL means there is no exit status; -1 keeps "no exit code" distinct
	// from "exited 1" in the log.
	if res.RC != -1 {
		t.Errorf("rc = %d, want -1 for a signalled process", res.RC)
	}
	if !strings.Contains(res.Wrap("cmd", 50*time.Millisecond, err).Error(), "timed out") {
		t.Errorf("wrapped error must name the timeout: %v", res.Wrap("cmd", 50*time.Millisecond, err))
	}
}

// A cancelled context is not the caller's deadline firing, so it gets its own
// outcome rather than being reported as a timeout.
func TestClassify_Cancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	runErr := cmd.Wait()

	res := Classify(ctx.Err(), runErr, cmd.ProcessState)
	if res.Outcome != OutcomeCancel {
		t.Errorf("outcome = %q, want %q", res.Outcome, OutcomeCancel)
	}
	if got := res.Wrap("cmd", time.Minute, runErr).Error(); !strings.Contains(got, "cancelled") {
		t.Errorf("wrapped error must say cancelled, got %v", got)
	}
}

// Both ways os/exec can fail to start a process mean the command never ran.
// An *exec.Error is a bare name that missed the PATH lookup; an *fs.PathError
// is a name with a separator passed straight to the kernel. Neither is the
// command having failed, and neither leaves a process state.
func TestClassify_Start(t *testing.T) {
	for _, name := range []string{
		"/nonexistent/binary",               // *fs.PathError
		"definitely-not-a-real-binary-xyzz", // *exec.Error, PATH lookup
	} {
		t.Run(name, func(t *testing.T) {
			res, err := run(t, 5*time.Second, name)
			if err == nil {
				t.Fatal("want an error for a missing binary")
			}
			if res.Outcome != OutcomeStart {
				t.Errorf("outcome = %q, want %q", res.Outcome, OutcomeStart)
			}
			if res.PID != 0 {
				t.Errorf("pid = %d, want 0: nothing was started", res.PID)
			}
			if got := res.Wrap("cmd", time.Minute, err).Error(); !strings.Contains(got, "could not start") {
				t.Errorf("wrapped error must say it could not start, got %v", got)
			}
		})
	}
}

// A nil process state must not panic: that is the "never started" case, and
// Attrs has to survive it because it runs on the same path as the log line.
func TestClassify_NilProcessState(t *testing.T) {
	res := Classify(nil, errors.New("boom"), nil)
	if res.Outcome != OutcomeExit {
		t.Errorf("outcome = %q, want %q for a non-start error", res.Outcome, OutcomeExit)
	}
	if res.RC != -1 {
		t.Errorf("rc = %d, want -1", res.RC)
	}
	if got := res.Attrs(time.Second, time.Minute, "cmd", "x"); len(got) == 0 {
		t.Error("Attrs must still return the caller's base attributes")
	}
}

// signalName must stay a no-op on states that were not signalled, and Classify
// must tolerate a WaitStatus it cannot interpret.
func TestSignalName(t *testing.T) {
	if got := signalName(mustState(t, "/bin/true")); got != "" {
		t.Errorf("signalName(clean exit) = %q, want empty", got)
	}
	if got := signalName(mustState(t, "/bin/sh", "-c", "kill -TERM $$")); got != "terminated" {
		t.Errorf("signalName(SIGTERM) = %q, want %q", got, "terminated")
	}
}

func mustState(t *testing.T, name string, args ...string) *os.ProcessState {
	t.Helper()
	cmd := exec.Command(name, args...)
	// A non-zero exit is expected for some of these; the process state is
	// what is under inspection either way.
	_ = cmd.Run()
	if cmd.ProcessState == nil {
		t.Fatalf("%s produced no process state", name)
	}
	return cmd.ProcessState
}

// Attrs is what the outcome log line is built from. Its shape is the contract
// with the log reader, so each rule is pinned here.
func TestAttrs(t *testing.T) {
	clean := Result{Outcome: OutcomeOK, PID: 42, RC: 0}
	got := render(clean.Attrs(time.Second, time.Minute, "cmd", "zypper"))
	for _, want := range []string{"cmd=zypper", "outcome=ok", "pid=42", "rc=0", "exit_code=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("clean exit missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "signal=") {
		t.Errorf("a process that was not signalled must omit signal, so its presence means something: %s", got)
	}
	if strings.Contains(got, "deadline=") {
		t.Errorf("a deadline that did not fire must not be logged: %s", got)
	}

	killed := Result{Outcome: OutcomeTimeout, PID: 43, RC: -1, Signal: "killed"}
	got = render(killed.Attrs(5*time.Minute, 5*time.Minute, "cmd", "zypper"))
	for _, want := range []string{"outcome=timeout", "signal=killed", "rc=-1", "exit_code=-1", "deadline=5m0s"} {
		if !strings.Contains(got, want) {
			t.Errorf("timeout missing %q: %s", want, got)
		}
	}
}

// render formats attributes through a real slog TextHandler, so assertions
// read exactly like the log lines an operator sees rather than through a
// hand-rolled approximation of that format.
func render(attrs []any) string {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		// ReplaceAttr drops the timestamp, which is the only field here with
		// no bearing on what is being tested.
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	logger.Info("", attrs...)
	return buf.String()
}

// A caller needs to be able to tell a timeout apart from any other failure
// programmatically, not just by reading the message: the buildroot records it
// as its own metric outcome. errors.Is against context.DeadlineExceeded is
// the contract that makes that possible, so it is pinned here.
func TestWrap_ErrorsIsIdentifiesTimeout(t *testing.T) {
	res, err := run(t, 50*time.Millisecond, "/bin/sleep", "30")
	if res.Outcome != OutcomeTimeout {
		t.Fatalf("outcome = %q, want timeout", res.Outcome)
	}
	wrapped := res.Wrap("cmd", 50*time.Millisecond, err)
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Errorf("errors.Is(wrapped, DeadlineExceeded) = false for %v", wrapped)
	}

	// A plain non-zero exit must not claim to be a timeout, or this label
	// would over-count.
	res, err = run(t, 5*time.Second, "/bin/false")
	wrapped = res.Wrap("cmd", time.Minute, err)
	if errors.Is(wrapped, context.DeadlineExceeded) {
		t.Errorf("a non-zero exit must not read as a timeout: %v", wrapped)
	}

	// A real exit status stays reachable for callers that want it.
	var exitErr *exec.ExitError
	if !errors.As(wrapped, &exitErr) {
		t.Errorf("the *exec.ExitError must stay reachable: %v", wrapped)
	}
}

// Cancellation is likewise distinguishable, and from a timeout.
func TestWrap_ErrorsIsIdentifiesCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	runErr := cmd.Wait()

	res := Classify(ctx.Err(), runErr, cmd.ProcessState)
	wrapped := res.Wrap("cmd", time.Minute, runErr)
	if !errors.Is(wrapped, context.Canceled) {
		t.Errorf("errors.Is(wrapped, Canceled) = false for %v", wrapped)
	}
	if errors.Is(wrapped, context.DeadlineExceeded) {
		t.Errorf("a cancellation must not read as a timeout: %v", wrapped)
	}
}
