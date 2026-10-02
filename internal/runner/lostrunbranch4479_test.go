package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// lostBranchDeterministic commits work on "implement", then deletes the run
// branch from the managed working copy on "lose-branch" (an external deletion
// or pruning fetch between stages). "review" must never run.
type lostBranchDeterministic struct {
	t      *testing.T
	stages []string
}

func (d *lostBranchDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.t.Helper()
	stage := strings.TrimPrefix(env.TaskID, env.RunID+":")
	d.stages = append(d.stages, stage)
	switch stage {
	case "implement":
		if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte("work\n"), 0o644); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		runGit(d.t, env.Workspace, "add", "-A")
		runGit(d.t, env.Workspace, "commit", "-m", "implement")
	case "lose-branch":
		branch := strings.TrimSpace(gitOutput(d.t, env.Workspace, "symbolic-ref", "--short", "HEAD"))
		runGit(d.t, env.Workspace, "checkout", "--detach")
		runGit(d.t, env.Workspace, "branch", "-D", branch)
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func lostBranchWorkflow(t *testing.T) *workflow.Machine {
	t.Helper()
	step := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: next,
		}
	}
	spec := apiv1.WorkflowSpec{
		Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start: "implement",
		Tasks: []apiv1.Task{
			step("implement", "lose-branch"),
			step("lose-branch", "review"),
			step("review", workflow.TerminalComplete),
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "lost-branch", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	return machine
}

// TestRunnerSurfacesLostRunBranchAsInfrastructureFailure is #4479's
// regression: a stage past the first whose run branch vanished fails the run
// as an infrastructure (workspace) failure instead of getting a fresh branch
// cut from base, whose empty diff would be blamed on the implementer.
func TestRunnerSurfacesLostRunBranchAsInfrastructureFailure(t *testing.T) {
	const runID = "run-lost-branch"
	deterministic := &lostBranchDeterministic{t: t}
	r, runsDir := newInfraRetryRunner(t, &infraRetryNoWorkGoober{t: t}, deterministic)
	var _ invoke.Deterministic = deterministic

	res, err := r.Start(context.Background(), salvageStartInput(runID, lostBranchWorkflow(t)))
	if !errors.Is(err, worktree.ErrRunBranchLost) {
		t.Fatalf("Start error = %v, want worktree.ErrRunBranchLost", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if want := []string{"implement", "lose-branch"}; strings.Join(deterministic.stages, ",") != strings.Join(want, ",") {
		t.Fatalf("stages run = %v, want %v (review must not run on a recreated branch)", deterministic.stages, want)
	}
	wantClass := string(telemetry.ClassifyError(worktree.RunBranchLostCode))
	if wantClass != string(telemetry.ErrorClassInfra) {
		t.Fatalf("%s classifies as %q, want infra", worktree.RunBranchLostCode, wantClass)
	}
	for _, event := range readRunEvents(t, runsDir, runID) {
		if event.Type != journal.EventError || event.Stage != "review" {
			continue
		}
		if event.Runner[stageErrorCodeKey] != worktree.RunBranchLostCode || event.Runner[stageErrorClassKey] != wantClass {
			t.Fatalf("review dispatch failure runner = %v, want code %s class %s", event.Runner, worktree.RunBranchLostCode, wantClass)
		}
		return
	}
	t.Fatal("no review dispatch failure journaled for the lost run branch")
}
