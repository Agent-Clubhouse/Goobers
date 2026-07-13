package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// ProcessRequest describes one harness subprocess execution.
type ProcessRequest struct {
	// Command is argv: Command[0] is the executable, the rest are arguments.
	Command []string
	// Dir is the working directory (the stage workspace).
	Dir string
	// Env is the full child environment. Nil means "inherit this process's
	// environment" (the os/exec default) — callers that need to add
	// capability tokens build on top of that explicitly (never a partial or
	// synthetic env, so nothing PATH-dependent breaks by surprise).
	Env []string
	// Timeout bounds the process; zero means no timeout.
	Timeout time.Duration
}

// ProcessResult is what a harness subprocess produced.
type ProcessResult struct {
	// Transcript is combined stdout+stderr.
	Transcript []byte
	// ExitCode is the process's exit code (0 on success; -1 if it never
	// started or was killed by a signal).
	ExitCode int
}

// ProcessRunner runs the concrete harness subprocess — the seam that lets
// CopilotAdapter be tested without a real Copilot CLI installed.
type ProcessRunner interface {
	Run(ctx context.Context, req ProcessRequest) (ProcessResult, error)
}

// ExecProcessRunner runs a harness command with os/exec.
type ExecProcessRunner struct{}

// Run executes req.Command in req.Dir with req.Env, capturing combined
// stdout+stderr as the transcript. A ProcessResult is always returned
// alongside an error (including on timeout) so the caller can still record a
// partial transcript as a journal span even when the harness fails.
func (ExecProcessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	if len(req.Command) == 0 {
		return ProcessResult{}, fmt.Errorf("harness: empty command")
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = req.Env

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	result := ProcessResult{Transcript: buf.Bytes(), ExitCode: -1}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.ExitCode = 0
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	}

	if ctx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("%w after %s: %s", ErrTimeout, req.Timeout, req.Command[0])
	}
	if err != nil {
		return result, fmt.Errorf("harness: run %v: %w", req.Command, err)
	}
	return result, nil
}
