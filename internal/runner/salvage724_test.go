package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// committingTimeoutGoober is an agentic stub that commits a real change to the
// run branch and then reports a session timeout (invoke.Timeout) — the #724
// shape: work was actually made and committed before the wall-clock kill.
type committingTimeoutGoober struct {
	t     *testing.T
	calls int
}

func (g *committingTimeoutGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.t.Helper()
	g.calls++
	// Unique content per attempt: a run's stages share one branch, so a retry
	// re-checks-out a worktree that already carries the prior attempt's commit —
	// writing the same bytes would leave nothing to commit.
	content := []byte(fmt.Sprintf("salvaged work, attempt %d\n", g.calls))
	if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), content, 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(g.t, env.Workspace, "add", "-A")
	runGit(g.t, env.Workspace, "commit", "-m", "wip committed before session timeout")
	return apiv1.ResultEnvelope{}, invoke.Timeout(errors.New("harness: copilot-cli: harness: session timed out after 30m0s"))
}

func (g *committingTimeoutGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("committingTimeoutGoober is not a reviewer")
}

// nonCommittingTimeoutGoober reports a session timeout without ever committing
// — a pre-commit timeout, which has no viable diff to salvage.
type nonCommittingTimeoutGoober struct{ calls int }

func (g *nonCommittingTimeoutGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.calls++
	return apiv1.ResultEnvelope{}, invoke.Timeout(errors.New("harness: session timed out after 30m0s"))
}

func (g *nonCommittingTimeoutGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("nonCommittingTimeoutGoober is not a reviewer")
}

// salvageMachine is an agentic implement stage → deterministic local-ci →
// complete, with the implement stage's OnTimeout configurable so a test can
// exercise both the salvage and the default-fail paths. Retry maxAttempts=2
// mirrors the real implementation workflow, so a non-salvaged timeout retries
// once before failing.
func salvageMachine(t *testing.T, onTimeout string) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce a diff",
				Retry:     &apiv1.RetryPolicy{MaxAttempts: 2},
				OnTimeout: onTimeout,
				Next:      "local-ci",
			},
			{
				Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "run make ci",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
				Next: workflow.TerminalComplete,
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "salvage-fixture", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile salvage machine: %v", err)
	}
	return m
}

// newSalvageRunner wires a Runner over the given agentic goober and a
// call-counting deterministic stub (reused across NewDeterministic calls) for
// the local-ci stage.
func newSalvageRunner(t *testing.T, goober invoke.Goober, localCI *countingDeterministic) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return localCI, nil
		},
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
	return r, runsDir
}

func salvageStartInput(runID string, m *workflow.Machine) StartInput {
	return StartInput{
		RunID:   runID,
		Machine: m,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}
}

// TestRunnerSalvagesAgenticTimeoutWithCommittedDiff is #724's headline: an
// implement session that times out after committing a viable diff completes the
// stage with that diff and advances to local-ci — no retry, no discarded run.
func TestRunnerSalvagesAgenticTimeoutWithCommittedDiff(t *testing.T) {
	goober := &committingTimeoutGoober{t: t}
	localCI := &countingDeterministic{}
	r, runsDir := newSalvageRunner(t, goober, localCI)

	res, err := r.Start(context.Background(), salvageStartInput("run-salvage", salvageMachine(t, apiv1.TaskOnTimeoutSalvage)))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed (salvaged timeout should advance the run)", res.Phase)
	}
	if goober.calls != 1 {
		t.Fatalf("implement invoked %d times, want 1 (salvage must not consume the retry budget)", goober.calls)
	}
	if localCI.calls != 1 {
		t.Fatalf("local-ci ran %d times, want 1 (the run must advance to the next stage after salvage)", localCI.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-salvage"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawMarker, sawSalvagedOutput bool
	for _, e := range events {
		if e.Type == journal.EventArtifactRecorded && strings.Contains(e.Name, "salvage-on-timeout") {
			sawMarker = true
		}
		if e.Type == journal.EventStageFinished && e.Stage == "implement" {
			if v, ok := e.Outputs["salvagedOnTimeout"].(bool); ok && v {
				sawSalvagedOutput = true
			}
		}
	}
	if !sawMarker {
		t.Fatal("no salvage-on-timeout provenance artifact recorded")
	}
	if !sawSalvagedOutput {
		t.Fatal("implement stage.finished missing salvagedOnTimeout=true output")
	}
}

// TestRunnerDoesNotSalvageTimeoutWithoutCommittedDiff: a pre-commit timeout has
// nothing viable to salvage, so OnTimeout=salvage falls back to the normal
// retry-then-fail path.
func TestRunnerDoesNotSalvageTimeoutWithoutCommittedDiff(t *testing.T) {
	goober := &nonCommittingTimeoutGoober{}
	localCI := &countingDeterministic{}
	r, _ := newSalvageRunner(t, goober, localCI)

	res, err := r.Start(context.Background(), salvageStartInput("run-no-salvage", salvageMachine(t, apiv1.TaskOnTimeoutSalvage)))
	if err == nil {
		t.Fatal("expected the exhausted, non-salvageable timeout to surface as a dispatch error")
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed (an empty diff has nothing to salvage)", res.Phase)
	}
	if goober.calls != 2 {
		t.Fatalf("implement invoked %d times, want 2 (a non-salvageable timeout still exhausts the retry budget)", goober.calls)
	}
	if localCI.calls != 0 {
		t.Fatalf("local-ci ran %d times, want 0 (the run must not advance past a non-salvaged timeout)", localCI.calls)
	}
}

// TestRunnerDoesNotSalvageWhenOnTimeoutDefault proves salvage is strictly
// opt-in: the same committing-then-timing-out goober, with OnTimeout unset,
// discards the attempt exactly as before #724.
func TestRunnerDoesNotSalvageWhenOnTimeoutDefault(t *testing.T) {
	goober := &committingTimeoutGoober{t: t}
	localCI := &countingDeterministic{}
	r, _ := newSalvageRunner(t, goober, localCI)

	res, err := r.Start(context.Background(), salvageStartInput("run-default", salvageMachine(t, "")))
	if err == nil {
		t.Fatal("expected the default (non-salvage) timeout to surface as a dispatch error")
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed (default OnTimeout must not salvage)", res.Phase)
	}
	if localCI.calls != 0 {
		t.Fatalf("local-ci ran %d times, want 0 (no salvage without opt-in)", localCI.calls)
	}
}
