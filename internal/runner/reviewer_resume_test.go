package runner

import (
	"context"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type resumedReviewer struct{ attempt int32 }

func (*resumedReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, nil
}
func (r *resumedReviewer) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.attempt = env.Attempt
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestReviewerResumePreservesInterruptedDispatchAndContinuesVisit(t *testing.T) {
	spec := apiv1.WorkflowSpec{Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "implement",
		Tasks: []apiv1.Task{{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "prepare", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"}},
		Gates: []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"}, Branches: map[string]string{"pass": workflow.TerminalComplete, "needs-changes": "implement", "fail": workflow.TargetAbort}}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "reviewer-recovery", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	runsDir, repo, worktrees := newTestRunnerEnv(t)
	run, err := journal.Create(runsDir, journal.RunIdentity{RunID: "reviewer-crash", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version, WorkflowDigest: machine.Digest(), Gaggle: spec.Gaggle, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "success"},
		{Type: journal.EventGateStarted, Gate: "review", Runner: map[string]any{"repassAttempt": 1}},
		journal.ReviewerAttemptEvent(journal.EventReviewerStarted, "review", 1, ""),
		{Type: journal.EventReviewerFinished, Stage: "review", Gate: "review", Attempt: 1, Status: "failure"},
		journal.ReviewerAttemptEvent(journal.EventReviewerStarted, "review", 2, journal.AttemptInfra),
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	run.SetMachineState("review")
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reviewer := &resumedReviewer{}
	runner, err := New(Config{RunsDir: runsDir, Worktrees: worktrees, RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{byTask: map[string]stubTaskResult{}}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return reviewer, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Resume(context.Background(), ResumeInput{RunID: "reviewer-crash", Machine: machine, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}})
	if err != nil || result.Phase != journal.PhaseCompleted || reviewer.attempt != 3 {
		t.Fatalf("resume=%+v attempt=%d err=%v", result, reviewer.attempt, err)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, "reviewer-crash"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var starts []journal.Event
	for _, event := range events {
		if event.Type == journal.EventReviewerStarted {
			starts = append(starts, event)
		}
	}
	if len(starts) != 3 || starts[1].Attempt != 2 || starts[2].Attempt != 3 || starts[2].AttemptClass != journal.AttemptInfra {
		t.Fatalf("resumed lifecycle=%+v", starts)
	}
	if starts[1].Seq >= starts[2].Seq || journal.StageAttemptID("reviewer-crash", 0, "review", starts[1].Seq) == journal.StageAttemptID("reviewer-crash", 0, "review", starts[2].Seq) {
		t.Fatal("recovery reused interrupted identity")
	}
}
