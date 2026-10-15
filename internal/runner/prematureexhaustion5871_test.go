package runner

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

// The harness authors the rewrite; the runner decides its retry class and
// telemetry its error class. The spellings must never drift apart.
func TestPrematureExhaustionClaimCodeMatchesHarness(t *testing.T) {
	if PrematureExhaustionClaimCode != harness.ErrorCodePrematureExhaustionClaim {
		t.Fatalf("runner %q != harness %q", PrematureExhaustionClaimCode, harness.ErrorCodePrematureExhaustionClaim)
	}
	if class := telemetry.ClassifyError(PrematureExhaustionClaimCode); class != telemetry.ErrorClassValidation {
		t.Fatalf("%s classifies as %q, want %q (an unbacked claim, not a timeout)", PrematureExhaustionClaimCode, class, telemetry.ErrorClassValidation)
	}
}

// #5871: an agent that gave up claiming its window was exhausted, long before
// the deadline, is sent back through the stage's own retry policy rather than
// failing the run on a false timeout.
func TestRunnerRetriesPrematureExhaustionClaimPerPolicy(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "prioritize",
		Tasks: []apiv1.Task{{
			Name: "prioritize", Type: apiv1.TaskAgentic, Goober: "curator", Goal: "curate the claimed batch",
			Retry: &apiv1.RetryPolicy{MaxAttempts: 2},
			Next:  workflow.TerminalComplete,
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "agentic-premature-exhaustion-retry", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	goober := &sequencedGoober{results: []apiv1.ResultEnvelope{
		{
			Status: apiv1.ResultFailure,
			Error: &apiv1.ErrorInfo{
				Code:      PrematureExhaustionClaimCode,
				Message:   "the agent reported EXECUTION_WINDOW_EXHAUSTED after 22s of its 30m0s execution window",
				Retryable: true,
			},
		},
		{Status: apiv1.ResultSuccess, Summary: "curated"},
	}}
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	r, err := New(Config{
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-agentic-premature-exhaustion-retry",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted || goober.callCount() != 2 {
		t.Fatalf("result=%+v calls=%d, want completed on the policy retry", res, goober.callCount())
	}
}
