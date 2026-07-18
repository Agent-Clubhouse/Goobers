package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// DefaultTimeout bounds an agentic harness session when the Executor has no
// timeout configured (#119): a hung Copilot CLI (network stall, or a
// flag-semantics regression that reintroduces an interactive prompt) must
// eventually be killed rather than blocking its run — and every run holding
// a max-parallel slot — forever. This matters more here than for a
// deterministic shell stage (internal/executor.DefaultTimeout) because the
// runner deliberately dispatches every attempt on a context.WithoutCancel
// (internal/runner/run.go's documented drain contract), so not even SIGTERM
// can reach a stuck agentic call — only this process-level timeout can.
const DefaultTimeout = 30 * time.Minute

// groupKillWaitDelay bounds how long Run waits for cmd.Wait() to return
// after killing the whole process group on timeout, in case a descendant
// escaped the group (e.g. via setsid) and is still holding a stdout/stderr
// pipe open — cmd.Wait() would otherwise never return, hanging the stage
// (and graceful drain) exactly as before the group-kill fix, just one layer
// down. Giving up after this bound lets the stage's own accounting proceed
// even though the escaped process may leak; there is no portable,
// unconditional way to guarantee its death.
const groupKillWaitDelay = 5 * time.Second

// DefaultMaxTranscriptBytes caps the combined stdout+stderr transcript a
// harness subprocess accumulates in memory (each of stdout/stderr can write
// into it) when ProcessRequest.MaxTranscriptBytes is unset (#245). Unlike
// internal/executor's ShellExecutor (1 MiB, deterministic commands with
// disciplined output), an agentic harness session's output is chattier and
// harder to predict, so the default sits at the upper end of the 1–4 MiB
// range the issue calls for.
const DefaultMaxTranscriptBytes int64 = 4 << 20 // 4 MiB

// syncBuffer is a mutex-guarded, size-capped byte sink. Run gives up waiting
// on cmd.Wait() after groupKillWaitDelay if a descendant escaped the process
// group and is still holding a pipe open — at that point os/exec's own
// stdout/stderr-copying goroutines may still be writing to this buffer, so
// both the cap and reading its contents (Bytes) must happen under the same
// mutex those goroutines write through, never racing them.
//
// Past limit, Write keeps returning len(p) (never blocks or errors the
// producing process — os/exec's copy goroutine must always be able to drain
// the pipe) but stops retaining bytes, only counting how many were dropped;
// Bytes appends a stable in-band marker once truncation occurred, so a
// chatty or looping agentic session can never balloon daemon memory or write
// an unbounded blob into the journal (#245).
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	limit   int64
	dropped int64
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.limit - int64(b.buf.Len())
	switch {
	case remaining <= 0:
		b.dropped += int64(n)
	case int64(n) > remaining:
		b.buf.Write(p[:remaining])
		b.dropped += int64(n) - remaining
	default:
		b.buf.Write(p)
	}
	return n, nil
}

// Bytes returns a snapshot of what's been captured so far, with a trailing
// "[transcript truncated: N bytes dropped]" marker appended if the cap was
// hit. Safe to call concurrently with Write (see the type doc for why that
// matters here).
func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := append([]byte(nil), b.buf.Bytes()...)
	if b.dropped > 0 {
		out = append(out, fmt.Sprintf("\n[transcript truncated: %d bytes dropped]\n", b.dropped)...)
	}
	return out
}

// Truncated reports whether the cap was hit. Safe to call concurrently with
// Write, for the same reason as Bytes.
func (b *syncBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped > 0
}

// Dropped returns the number of bytes discarded past the cap. Safe to call
// concurrently with Write, for the same reason as Bytes.
func (b *syncBuffer) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// ProcessRequest describes one harness subprocess execution.
type ProcessRequest struct {
	// Command is argv: Command[0] is the executable, the rest are arguments.
	Command []string
	// Dir is the working directory (the stage workspace).
	Dir string
	// Env is the full child environment. Nil or empty means NO environment at
	// all (#122) — the opposite of os/exec's own default, which would
	// silently inherit this daemon process's full environment (SEC-045: any
	// resolver-sourced credential env var the daemon happens to hold would
	// leak into every subprocess regardless of declared capabilities). A
	// caller that wants PATH/HOME/etc. must build that explicitly (baseEnv()
	// in this package, or internal/executor's identical allowlist) — never a
	// passthrough by omission.
	Env []string
	// Timeout bounds the process; zero means no timeout.
	Timeout time.Duration
	// MaxTranscriptBytes caps the combined stdout+stderr transcript retained
	// in memory; non-positive means DefaultMaxTranscriptBytes (#245).
	MaxTranscriptBytes int64
}

// ProcessResult is what a harness subprocess produced.
type ProcessResult struct {
	// Transcript is combined stdout+stderr, bounded at MaxTranscriptBytes —
	// a truncated transcript carries a trailing marker (#245), never a
	// silently cut-off blob.
	Transcript []byte
	// ExitCode is the process's exit code (0 on success; -1 if it never
	// started or was killed by a signal).
	ExitCode int
	// TranscriptTruncated reports whether Transcript was capped.
	TranscriptTruncated bool
	// TranscriptDroppedBytes is how many transcript bytes were discarded past
	// the cap (0 if TranscriptTruncated is false).
	TranscriptDroppedBytes int64
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
//
// The command runs in its own SESSION (Setsid) so a timeout kills the whole
// subprocess tree, not just the direct child (#119) — an agent-spawned
// grandchild (a dev server, a test watcher) would otherwise survive the
// direct child's death and keep holding the stage's stdout/stderr pipes
// open, which would in turn keep cmd.Wait() from ever returning.
//
// Setsid, not Setpgid: a bare Setpgid child of a `goobers up` running in the
// foreground of an interactive terminal is a *background process group on
// that controlling terminal*, which the kernel STOPS (SIGTTOU/SIGTTIN, state
// T, zero CPU) the moment it touches terminal state — the "local-ci hang"
// #846 fixed in the executor's stage spawn (internal/executor/shell.go). This
// is the twin agentic-harness spawn path (the one that launches copilot);
// Setsid detaches the controlling terminal entirely so job control can't
// freeze it. The session leader's pgid == pid, so the timeout path's
// process-group kill (syscall.Kill(-cmd.Process.Pid, ...)) below is unchanged.
func (ExecProcessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	if len(req.Command) == 0 {
		return ProcessResult{}, fmt.Errorf("harness: empty command")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	cmd.Dir = req.Dir
	// A nil Env would make os/exec inherit the daemon's full environment —
	// exactly the SEC-045 fail-open default #122 flags. An explicit non-nil,
	// possibly-empty slice always wins instead, so "no Env supplied" means
	// "no environment", never "whatever the daemon happens to hold".
	env := req.Env
	if env == nil {
		env = []string{}
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	limit := req.MaxTranscriptBytes
	if limit <= 0 {
		limit = DefaultMaxTranscriptBytes
	}
	buf := syncBuffer{limit: limit}
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		return ProcessResult{ExitCode: -1}, fmt.Errorf("harness: start %v: %w", req.Command, err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var timedOut, canceled bool
	var err error
	select {
	case err = <-waitDone:
	case <-runCtx.Done():
		// runCtx.Done() fires both when its own timeout elapses and when the
		// caller's ctx is canceled out from under it — distinguishing the two
		// via context.Cause matters even though only the timeout path is
		// reachable today (internal/runner's dispatch always uses
		// context.WithoutCancel): a future hard-shutdown path that DOES
		// cancel ctx must not be mislabeled as a retryable timeout (#122).
		if errors.Is(context.Cause(runCtx), context.DeadlineExceeded) {
			timedOut = true
		} else {
			canceled = true
		}
		// Kill the whole process group (negative pid), not just the direct
		// child, so a runaway subprocess tree can't outlive the stage.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case err = <-waitDone:
		case <-time.After(groupKillWaitDelay):
			// A descendant escaped the group and is still holding a pipe
			// open; give up waiting rather than hang the stage (and drain)
			// forever — see groupKillWaitDelay's doc.
		}
	}

	result := ProcessResult{
		Transcript:             buf.Bytes(),
		ExitCode:               -1,
		TranscriptTruncated:    buf.Truncated(),
		TranscriptDroppedBytes: buf.Dropped(),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil && !timedOut && !canceled:
		result.ExitCode = 0
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	}

	if timedOut {
		return result, fmt.Errorf("%w after %s: %s", ErrTimeout, timeout, req.Command[0])
	}
	if canceled {
		return result, fmt.Errorf("%w: %s", ErrCanceled, req.Command[0])
	}
	if err != nil {
		return result, fmt.Errorf("harness: run %v: %w", req.Command, err)
	}
	return result, nil
}
