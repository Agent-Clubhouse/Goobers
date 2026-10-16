package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// realExitError returns a genuine *exec.ExitError from a process that exited
// non-zero — the test binary's own preflight-args fixture refusing an
// obsolete flag (TestMain) — so the classification is exercised against the
// error type process.go actually wraps, not a hand-built stand-in.
func realExitError(t *testing.T) error {
	t.Helper()
	err := exec.Command(os.Args[0], preflightArgsLauncherArg, "--obsolete-preflight").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("fixture process error = %v, want *exec.ExitError", err)
	}
	return err
}

// TestExecutorReviewClassifiesSessionEndWithoutVerdict is #5543: a reviewer
// session that ended without a verdict because its harness process exited
// non-zero, or because it timed out, must cross the invoke seam as an
// infrastructure failure — the class the gate's declared evaluator retry
// bound (retry.maxAttempts) applies to — exactly as a session that wrote no
// verdict file already does. Failures that would recur on a fresh session
// stay unmarked so they still fail fast.
func TestExecutorReviewClassifiesSessionEndWithoutVerdict(t *testing.T) {
	exitErr := realExitError(t)
	tests := []struct {
		name  string
		err   error
		infra bool
	}{
		{"process exited non-zero", fmt.Errorf("harness: run [copilot -p=review]: %w", exitErr), true},
		{"session timed out", fmt.Errorf("%w after 40m0s: copilot", ErrTimeout), true},
		{"no completion file", fmt.Errorf("%w: .goobers/verdict.json", ErrNoCompletion), true},
		{"verdict refused by schema", fmt.Errorf("%w: missing decision", ErrInvalidCompletion), false},
		{"required MCP rejected", errors.Join(fmt.Errorf("harness: run [copilot]: %w", exitErr), errRequiredMCPRejected), false},
		{"required MCP enterprise-blocked", errors.Join(fmt.Errorf("harness: run [copilot]: %w", exitErr), errRequiredMCPEnterpriseBlocked), false},
		{"declared required MCP failed to start", &declaredRequiredMCPStartupError{err: fmt.Errorf("harness: run [codex exec]: %w", exitErr)}, false},
		{"session canceled", fmt.Errorf("%w: copilot", ErrCanceled), false},
		{"unmarked harness refusal", errors.New("harness: unsupported option"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeRecorder{}
			adapter := &FakeAdapter{Act: func(context.Context, RunRequest) error { return tc.err }}
			executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
			if err != nil {
				t.Fatalf("NewExecutor: %v", err)
			}
			_, err = executor.Review(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
			if err == nil {
				t.Fatal("Review: want an error when the session ended without a verdict")
			}
			if got := invoke.IsInfrastructureFailure(err); got != tc.infra {
				t.Fatalf("Review error %v: infrastructure = %v, want %v", err, got, tc.infra)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("Review error %v does not preserve the harness cause %v", err, tc.err)
			}
			if errors.Is(tc.err, ErrTimeout) && !invoke.IsTimeout(err) {
				t.Fatalf("Review error %v lost its invoke.IsTimeout marker", err)
			}
		})
	}
}

// TestExecutorInvokeKeepsProcessExitUnmarked pins #5543's scope: only a
// REVIEW reclassifies a harness process exit. A task stage's failure stays
// the stage's own outcome — its retry and OnTimeout salvage semantics are
// owned elsewhere and must not change underneath it.
func TestExecutorInvokeKeepsProcessExitUnmarked(t *testing.T) {
	rec := &fakeRecorder{}
	cause := fmt.Errorf("harness: run [copilot -p=implement]: %w", realExitError(t))
	adapter := &FakeAdapter{Act: func(context.Context, RunRequest) error { return cause }}
	executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	_, err = executor.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err == nil || invoke.IsInfrastructureFailure(err) {
		t.Fatalf("Invoke error = %v, want an unmarked stage failure", err)
	}
}
