package runner

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
)

// TestCapabilityUnsatisfiedEndsRunFailedNotEscalated is #2197's runner-side
// acceptance: the harness reports a missing tool capability as a failure, so
// the run terminates PhaseFailed and takes Config.Failed's comment-only,
// no-label release path for every claimed item — never Config.Blocked, whose
// escalation needs-human-parks the whole claimed batch for a system defect
// none of those items caused.
func TestCapabilityUnsatisfiedEndsRunFailedNotEscalated(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-capability-2197:implement": {
			status: apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{
				Code:      harness.ErrorCodeCapabilityUnsatisfied,
				Message:   `declared capability "github:issues:write" is unsatisfiable`,
				Retryable: false,
			},
		},
	}
	r, _ := newTestRunner(t, byTask, nil)

	var failed FailedOutcome
	var failedCalls, blockedCalls int
	r.cfg.Failed = func(_ context.Context, o FailedOutcome) error {
		failedCalls++
		failed = o
		return nil
	}
	r.cfg.Blocked = func(context.Context, BlockedOutcome) error {
		blockedCalls++
		return nil
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-capability-2197",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed (escalated parks the whole claimed batch)", res.Phase)
	}
	if blockedCalls != 0 {
		t.Fatalf("Config.Blocked calls = %d, want 0", blockedCalls)
	}
	if failedCalls != 1 {
		t.Fatalf("Config.Failed calls = %d, want exactly 1", failedCalls)
	}
	if !strings.Contains(failed.Cause, harness.ErrorCodeCapabilityUnsatisfied) {
		t.Fatalf("FailedOutcome.Cause = %q, want it to name the capability failure", failed.Cause)
	}
}
