// Package execout classifies process completion status and formats outcome attributes for logging.
package execout

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Outcome represents how a process completed.
type Outcome string

const (
	// OutcomeOK indicates process exited with status 0.
	OutcomeOK Outcome = "ok"
	// OutcomeExit indicates process completed with non-zero exit code.
	OutcomeExit Outcome = "exit"
	// OutcomeTimeout indicates the execution deadline expired.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeCancel indicates the context was canceled before completion.
	OutcomeCancel Outcome = "cancel"
	// OutcomeStart indicates the process failed to execute (e.g. binary not found).
	OutcomeStart Outcome = "start"
)

// Result contains classified process termination details.
type Result struct {
	Outcome Outcome
	PID     int
	RC      int
	Signal  string
}

// Classify determines the process outcome from context error, exec error, and process state.
// Context errors are evaluated first because exec.CommandContext terminates timeouts via SIGKILL.
func Classify(ctxErr, runErr error, ps *os.ProcessState) Result {
	r := Result{RC: -1}

	if ps != nil {
		r.PID = ps.Pid()
		r.RC = ps.ExitCode()
		r.Signal = signalName(ps)
	}

	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		r.Outcome = OutcomeTimeout
	case errors.Is(ctxErr, context.Canceled):
		r.Outcome = OutcomeCancel
	case runErr != nil:
		var execErr *exec.Error
		var pathErr *fs.PathError
		if errors.As(runErr, &execErr) || errors.As(runErr, &pathErr) {
			r.Outcome = OutcomeStart
		} else {
			r.Outcome = OutcomeExit
		}
	default:
		r.Outcome = OutcomeOK
	}
	return r
}

// Attrs formats execution result attributes, appending them to base.
func (r Result) Attrs(duration, deadline time.Duration, base ...any) []any {
	attrs := make([]any, 0, len(base)+10)
	attrs = append(attrs, base...)
	attrs = append(attrs, "outcome", string(r.Outcome))
	if r.PID != 0 {
		attrs = append(attrs, "pid", r.PID)
	}
	attrs = append(attrs, "rc", r.RC)
	if r.Signal != "" {
		attrs = append(attrs, "signal", r.Signal)
	}
	if duration > 0 {
		attrs = append(attrs, "duration", duration)
	}
	if r.Outcome == OutcomeTimeout && deadline > 0 {
		attrs = append(attrs, "deadline", deadline)
	}
	return attrs
}

// Wrap returns an error describing the classified outcome, preserving unwrapping compatibility.
func (r Result) Wrap(prefix string, deadline time.Duration, runErr error) error {
	if runErr == nil {
		return nil
	}
	switch r.Outcome {
	case OutcomeTimeout:
		if deadline > 0 {
			return fmt.Errorf("%s timed out after %v (%w): %w", prefix, deadline, context.DeadlineExceeded, runErr)
		}
		return fmt.Errorf("%s timed out (%w): %w", prefix, context.DeadlineExceeded, runErr)
	case OutcomeCancel:
		return fmt.Errorf("%s was cancelled (%w): %w", prefix, context.Canceled, runErr)
	case OutcomeStart:
		return fmt.Errorf("%s could not start: %w", prefix, runErr)
	default:
		return fmt.Errorf("%s failed (rc %d): %w", prefix, r.RC, runErr)
	}
}

// signalName returns the signal name that terminated ps, or "" if not signaled.
func signalName(ps *os.ProcessState) string {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	return ws.Signal().String()
}
