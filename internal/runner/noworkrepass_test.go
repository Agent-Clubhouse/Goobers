package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// Regression coverage for #5107: "implementation: repass no-work after
// failed local-ci completes the run silently (no PR, no park) and
// re-releases the item." Reproduces the exact production shape (see
// reference-workflows/gaggles/goobers/workflows/implementation.yaml's
// review/local-gate pair) end to end through a real Runner.Start():
// implement succeeds once, review passes it through to local-ci, local-ci
// fails, local-gate's `fail` branch repasses straight back to implement
// (bypassing review), and the implementer — correctly finding nothing left
// to change — reports ResultNoWork with no new commit. Before the fix this
// fell through taskOutcome's unconditional finishNoWork(...) and completed
// the run as a healthy empty tick; the fix routes it through review instead,
// whose own duplicateDiff machinery (internal/gate/evaluate.go) escalates it.

// noworkRepassImplementer is the "implement" goober: attempt 1 commits a
// real change and succeeds; the repass attempt (dispatched only because
// local-gate sent local-ci's failure back here) makes no further change and
// reports ResultNoWork — the implementer correctly declining to redo work it
// already did, not a query stage finding nothing to do.
type noworkRepassImplementer struct {
	t     *testing.T
	calls int
}

func (g *noworkRepassImplementer) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.t.Helper()
	g.calls++
	if g.calls == 1 {
		if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte("first implementation\n"), 0o644); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		runGit(g.t, env.Workspace, "add", "-A")
		runGit(g.t, env.Workspace, "commit", "-m", "implement feature")
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}, nil
	}
	// The repass: local-ci already failed and local-gate routed it back
	// here. Nothing further to change — attempt 1's diff is still correct.
	return apiv1.ResultEnvelope{Status: apiv1.ResultNoWork, Summary: "nothing further to change"}, nil
}

func (g *noworkRepassImplementer) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.t.Fatal("implementer goober must never be invoked as a reviewer")
	return apiv1.Verdict{}, nil
}

// noworkRepassReviewer is the review gate's reviewer. It must be called
// exactly once: the second arrival at `review` (routed there by the #5107
// fix) reproduces attempt 1's identical diff byte-for-byte, so the gate's
// own duplicateDiff fast path resolves it WITHOUT a second reviewer call —
// asserting that stays true is as important as the routing itself.
type noworkRepassReviewer struct {
	calls int
}

func (g *noworkRepassReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, nil
}

func (g *noworkRepassReviewer) Review(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.calls++
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

// noworkRepassLocalCI is the local-ci deterministic stub: always fails, so
// local-gate always takes its `fail` branch.
type noworkRepassLocalCI struct {
	t     *testing.T
	calls int
}

func (d *noworkRepassLocalCI) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.t.Helper()
	d.calls++
	return apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "local_ci_failed", Message: "tests failed"}}, nil
}

// noworkRepassLocalGate is local-gate's automated evaluator stub: always
// reports "fail" — #5107's production local-gate (check: failure-class)
// does the same for a genuine, non-infrastructure local-ci failure.
type noworkRepassLocalGate struct{}

func (noworkRepassLocalGate) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	return "fail", nil
}

func noworkRepassWorkflow(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start: "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer", Goal: "implement",
				Next: "review",
			},
			{
				Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "verify",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "local-gate",
			},
		},
		Gates: []apiv1.Gate{
			{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					string(apiv1.VerdictPass):         "local-ci",
					string(apiv1.VerdictNeedsChanges): "implement",
					string(apiv1.VerdictFail):         workflow.TargetAbort,
					// Declared explicitly (rather than relying on
					// resolveOutcome's reserved-@escalate fallback) so this
					// test proves branch-specific routing through review's
					// own escalate control branch, exactly as production's
					// review -> park-escalated does.
					string(apiv1.VerdictEscalate): workflow.TargetEscalate,
				},
			},
			{
				Name: "local-gate", Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "always-fail"},
				Branches: map[string]string{
					"fail": "implement",
					"pass": workflow.TerminalComplete,
				},
			},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "nowork-repass-5107", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	return machine
}

func TestRunnerRoutesLocalCIRepassNoWorkThroughReviewEscalation(t *testing.T) {
	implementer := &noworkRepassImplementer{t: t}
	reviewer := &noworkRepassReviewer{}
	localCI := &noworkRepassLocalCI{t: t}

	instanceRoot := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	const runID = "run-nowork-repass-5107"
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return localCI, nil
		},
		NewAgentic: func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			if name == "reviewer" {
				return reviewer, nil
			}
			return implementer, nil
		},
		Automated:    noworkRepassLocalGate{},
		Worktrees:    manager,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	res, err := r.Start(context.Background(), salvageStartInput(runID, noworkRepassWorkflow(t)))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The bug's exact symptom: a repass no-work silently completing the run
	// (no PR, no park, no escalation) and re-releasing the claim.
	if res.Phase == journal.PhaseCompleted {
		t.Fatalf("phase = %q, want anything but completed (#5107 regression: a local-ci repass's no-work verdict must not silently complete the run)", res.Phase)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", res.Phase)
	}
	if reviewer.calls != 1 {
		t.Fatalf("reviewer calls = %d, want 1 (duplicateDiff fast path must skip the second reviewer call)", reviewer.calls)
	}
	if implementer.calls != 2 {
		t.Fatalf("implement dispatches = %d, want 2", implementer.calls)
	}
	if localCI.calls != 1 {
		t.Fatalf("local-ci dispatches = %d, want 1", localCI.calls)
	}

	events := readRunEvents(t, runsDir, runID)
	var reviewEvals []journal.Event
	for _, event := range events {
		if event.Type == journal.EventGateEvaluated && event.Gate == "review" {
			reviewEvals = append(reviewEvals, event)
		}
	}
	if len(reviewEvals) != 2 {
		t.Fatalf("review gate.evaluated events = %d, want 2 (got %+v)", len(reviewEvals), reviewEvals)
	}
	if dup, _ := reviewEvals[1].Runner["duplicateDiff"].(bool); !dup {
		t.Fatalf("second review evaluation duplicateDiff = %v, want true (runner=%+v)", reviewEvals[1].Runner["duplicateDiff"], reviewEvals[1].Runner)
	}
	if dup, _ := reviewEvals[0].Runner["duplicateDiff"].(bool); dup {
		t.Fatalf("first review evaluation duplicateDiff = %v, want false (runner=%+v)", reviewEvals[0].Runner["duplicateDiff"], reviewEvals[0].Runner)
	}
}
