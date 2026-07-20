package runner

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// alwaysErrDeterministic fails every dispatch with a fixed Go error (a
// dispatch-level failure, not a business ResultStatus) — the exact shape of the
// #1054 harness session timeout, which surfaces as a runTask error routed to
// failTerminal (the walk-level PhaseFailed path).
type alwaysErrDeterministic struct{ err error }

func (s alwaysErrDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, s.err
}

// TestRunnerFailedTerminalNotifiesHandlerOnWalkLevelFailure is #1054's core
// acceptance: a run that ends terminal PhaseFailed via the walk-level path (a
// dispatch-level harness error exhausting its attempts, e.g. a copilot-cli
// session timeout) invokes Config.Failed exactly once, carrying the run id, the
// target repo, the executing stage, and the terminal cause — so a human-visible
// trace can be left on the driving item instead of the issue silently returning
// to ready.
func TestRunnerFailedTerminalNotifiesHandlerOnWalkLevelFailure(t *testing.T) {
	machine := terminalFailMachine(t)
	timeoutErr := errors.New("harness: copilot-cli: harness: session timed out after 30m0s: copilot")
	r, _ := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return alwaysErrDeterministic{err: timeoutErr}, nil
	}, nil)

	var got FailedOutcome
	var calls int
	r.cfg.Failed = func(_ context.Context, o FailedOutcome) error {
		calls++
		got = o
		return nil
	}

	repoRef := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-failed-1054",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: repoRef,
	})
	if err == nil {
		t.Fatal("expected Start to surface the terminal failure error")
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if calls != 1 {
		t.Fatalf("Config.Failed calls = %d, want exactly 1", calls)
	}
	if got.RunID != "run-failed-1054" {
		t.Fatalf("FailedOutcome.RunID = %q, want run-failed-1054", got.RunID)
	}
	if got.RepoRef != repoRef {
		t.Fatalf("FailedOutcome.RepoRef = %+v, want %+v", got.RepoRef, repoRef)
	}
	if got.Stage != "implement" {
		t.Fatalf("FailedOutcome.Stage = %q, want implement", got.Stage)
	}
	if !strings.Contains(got.Cause, "session timed out after 30m0s") {
		t.Fatalf("FailedOutcome.Cause = %q, want it to carry the harness-timeout cause", got.Cause)
	}
}

// TestRunnerFailedTerminalNotifiesHandlerOnStageFailure proves the hook also
// fires for a stage-reported terminal failure (the finishStageFailure path),
// carrying the stage's own code+message as the cause.
func TestRunnerFailedTerminalNotifiesHandlerOnStageFailure(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-stagefail-1054:implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "executor_error", Message: "harness: copilot-cli: session timed out after 30m0s"},
		},
	}
	r, _ := newTestRunner(t, byTask, nil)

	var got FailedOutcome
	var calls int
	r.cfg.Failed = func(_ context.Context, o FailedOutcome) error {
		calls++
		got = o
		return nil
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-stagefail-1054",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if calls != 1 {
		t.Fatalf("Config.Failed calls = %d, want exactly 1", calls)
	}
	if got.Stage != "implement" {
		t.Fatalf("FailedOutcome.Stage = %q, want implement", got.Stage)
	}
	if !strings.Contains(got.Cause, "executor_error") || !strings.Contains(got.Cause, "session timed out") {
		t.Fatalf("FailedOutcome.Cause = %q, want it to carry the stage code+message", got.Cause)
	}
}

// TestRunnerFailedHandlerDoesNotFireOnNonFailedTerminals proves the hook is
// scoped to terminal PhaseFailed specifically — a completed, escalated, or
// aborted run must NOT invoke Config.Failed (those terminals have their own
// surfaces; a needs-human park stays reserved for escalation).
func TestRunnerFailedHandlerDoesNotFireOnNonFailedTerminals(t *testing.T) {
	cases := []struct {
		name      string
		next      string
		wantPhase journal.RunPhase
	}{
		{"completed", workflow.TerminalComplete, journal.PhaseCompleted},
		{"escalated", workflow.TargetEscalate, journal.PhaseEscalated},
		{"aborted", workflow.TargetAbort, journal.PhaseAborted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := taskReservedNextFixtureMachine(t, tc.next)
			runID := "run-nofire-" + tc.name
			byTask := map[string]stubTaskResult{runID + ":implement": {status: apiv1.ResultSuccess, summary: "done"}}
			r, _ := newTestRunner(t, byTask, nil)

			var called bool
			r.cfg.Failed = func(context.Context, FailedOutcome) error {
				called = true
				return nil
			}

			res, err := r.Start(context.Background(), StartInput{
				RunID:   runID,
				Machine: machine,
				Gaggle:  "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if res.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", res.Phase, tc.wantPhase)
			}
			if called {
				t.Fatalf("Config.Failed fired on a %s terminal, want it to fire only on failed", tc.name)
			}
		})
	}
}

// TestRunnerFailedHandlerErrorStillTerminal proves the Failed handler is
// best-effort like Blocked/RateLimited: its own error is journaled
// (failed_handling_failed) but never blocks the run from reaching PhaseFailed.
func TestRunnerFailedHandlerErrorStillTerminal(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-failed-herr:implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "executor_error", Message: "boom"},
		},
	}
	r, runsDir := newTestRunner(t, byTask, nil)
	r.cfg.Failed = func(context.Context, FailedOutcome) error {
		return errors.New("provider unreachable")
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-failed-herr",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed even though the handler errored", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-failed-herr"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawHandlerFailure bool
	for _, e := range events {
		if e.Type == journal.EventError && e.Error != nil && e.Error.Code == "failed_handling_failed" {
			sawHandlerFailure = true
		}
	}
	if !sawHandlerFailure {
		t.Fatal("expected a failed_handling_failed error event recording the handler's own failure")
	}
}
