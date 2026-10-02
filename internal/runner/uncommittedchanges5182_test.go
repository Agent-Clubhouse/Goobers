package runner

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// The harness produces the failure; the runner decides its retry class. The
// two spellings must never drift apart.
func TestUncommittedChangesCodeMatchesHarness(t *testing.T) {
	if UncommittedChangesCode != harness.ErrorCodeUncommittedChanges {
		t.Fatalf("runner %q != harness %q", UncommittedChangesCode, harness.ErrorCodeUncommittedChanges)
	}
}

// #5182: a writable stage that reported success with its work uncommitted is
// sent back through its own retry policy instead of reaching the reviewer gate
// as an empty diff.
func TestRunnerRetriesUncommittedChangesPerPolicy(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "commit a change",
			Retry: &apiv1.RetryPolicy{MaxAttempts: 2},
			Next:  workflow.TerminalComplete,
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "agentic-uncommitted-retry", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	goober := &sequencedGoober{results: []apiv1.ResultEnvelope{
		{
			Status: apiv1.ResultFailure,
			Error: &apiv1.ErrorInfo{
				Code:      UncommittedChangesCode,
				Message:   "UNCOMMITTED_CHANGES: the stage reported success but left its changes uncommitted",
				Retryable: true,
			},
		},
		{Status: apiv1.ResultSuccess, Summary: "committed"},
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
		RunID:   "run-agentic-uncommitted-retry",
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
