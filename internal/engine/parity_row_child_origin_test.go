package engine

// Parity row ECW-child-origin (HA-P4-08): a stage that may request child
// workflows binds its child authority to the committed stage start on both
// sides, and dispatch carries exactly that origin.
//
// The runner binds in recordTaskStarted (AppendChildStageStarted) and pins
// the origin in pinPublicationAuthority. The engine asks the live writer to
// bind the same start (bindChildStart), keeps the per-op receipt from the emit
// response, and carries it in taskAttemptEnvelope. Sequence numbers differ by
// backend, so the row compares the journal relation: each origin is the
// StageAttemptID of its own committed start, never a projection ordinal.
//
// The row is standalone rather than in parityRegistry because the registry's
// engine side runs production Run, which still refuses child parents (#7758),
// and the runner side needs the contained-parent owners configured below.

import (
	"context"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// noChildHandoff never observes a request, so the parent stage completes
// without yielding. It is the runner's "child-capable, no child" path.
type noChildHandoff struct{}

func (noChildHandoff) Await(ctx context.Context, _ apiv1.InvocationEnvelope) (runner.ChildHandoffRequest, error) {
	<-ctx.Done()
	return runner.ChildHandoffRequest{}, ctx.Err()
}
func (noChildHandoff) Yield(context.Context, runner.ChildHandoffRequest, runner.ChildWorkspaceCustody) error {
	return nil
}
func (noChildHandoff) Wait(context.Context, runner.ChildHandoffRequest) (runner.ChildHandoffCompletion, error) {
	return runner.ChildHandoffCompletion{}, nil
}

type noChildCapacity struct{}

func (noChildCapacity) SuspendChildParent(context.Context, string) (runner.ChildParentSuspension, error) {
	return nil, nil
}

func childOriginParitySpec() apiv1.WorkflowSpec {
	t := apiv1.Task{Name: "parent", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "parent", Workspace: apiv1.WorkspaceRepo, Next: wf.TerminalComplete,
		ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}
	return fixtureSpec("parent", []apiv1.Task{t}, nil)
}

func childOriginRunnerSide(t *testing.T, runID string) ([]journal.Event, []apiv1.InvocationEnvelope, error) {
	t.Helper()
	exec := newRecordingExec(map[string][]scriptedCall{"parent": {succeed(nil)}})
	root := t.TempDir()
	workcopies, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	repo := newConformanceFixtureRepo(t)
	r, err := runner.New(runner.Config{
		InstanceID: parityInstanceID,
		NewAgentic: func(goober string, _ runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Goober, error) {
			return exec.boundTo(goober), nil
		},
		Automated:            gate.NewAutomatedEvaluator(),
		Worktrees:            workcopies,
		RunsDir:              filepath.Join(root, "runs"),
		ScratchDir:           filepath.Join(root, "scratch"),
		RepoCloneURL:         func(apiv1.RepoRef) (string, error) { return repo, nil },
		AdmitParentExecution: func(context.Context, *wf.Machine) error { return nil },
		ChildHandoff:         noChildHandoff{},
		ChildParentCapacity:  noChildCapacity{},
		BorrowParentJournal:  func(string, string, *journal.Run) (func(), error) { return func() {}, nil },
		RestoreParentArchive: func(context.Context, runner.OwnedJournalRecorder, runner.ParentWorkspaceArchive, uint64) error {
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := wf.Compile(wf.Definition{Name: "child-origin", Version: 1, DSLVersion: "3.1", Spec: childOriginParitySpec()}, wf.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := r.Start(context.Background(), runner.StartInput{RunID: runID, Machine: machine, Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: parityRepoRef})
	exec.mu.Lock()
	defer exec.mu.Unlock()
	return readJournalEvents(t, filepath.Join(root, "runs", runID)), append([]apiv1.InvocationEnvelope(nil), exec.envs...), runErr
}

func childOriginEngineSide(t *testing.T, runID string) ([]journal.Event, []apiv1.InvocationEnvelope, error) {
	t.Helper()
	f := newChildFixture()
	f.handoffs = 0
	env, dir := childEnvironmentWithEntry(t, f, Run)
	in := runInput("child-origin", childOriginParitySpec())
	in.RunID, in.DSLVersion, in.LiveJournal, in.RepoRef = runID, "3.1", true, parityRepoRef
	in.ChildWorkflowsEnabled = true
	env.ExecuteWorkflow("Run", in)
	return liveEvents(t, dir, runID), f.envelopes, env.GetWorkflowError()
}

// committedOriginRelation returns the origin bound to the first parent start
// and checks it is derived from that start's own committed sequence.
func committedOriginRelation(t *testing.T, side, runID string, events []journal.Event) apiv1.ChildWorkflowOrigin {
	t.Helper()
	for _, event := range events {
		if event.Type != journal.EventStageStarted || event.Stage != "parent" {
			continue
		}
		origin, err := journal.ChildWorkflowOriginForEvent(runID, event)
		if err != nil {
			t.Fatalf("%s: parent start carries no committed child origin: %v", side, err)
		}
		want := journal.StageAttemptID(runID, 0, "parent", event.Seq)
		if origin.StageOccurrence != want || origin.AttemptID != want {
			t.Fatalf("%s: origin %+v is not the identity of committed seq %d", side, origin, event.Seq)
		}
		return *origin
	}
	t.Fatalf("%s: no parent stage start", side)
	return apiv1.ChildWorkflowOrigin{}
}

func TestParityRowChildOriginCommittedBinding(t *testing.T) {
	const runID = "run-child-origin"
	runnerEvents, runnerEnvs, runnerErr := childOriginRunnerSide(t, runID)
	engineEvents, engineEnvs, engineErr := childOriginEngineSide(t, runID)
	if runnerErr != nil || engineErr != nil {
		t.Fatalf("walk outcome differs or failed: runner=%v engine=%v", runnerErr, engineErr)
	}
	// Premise: the runner really binds the start and dispatches its origin.
	runnerOrigin := committedOriginRelation(t, "runner", runID, runnerEvents)
	if len(runnerEnvs) != 1 || runnerEnvs[0].ChildWorkflowOrigin == nil || *runnerEnvs[0].ChildWorkflowOrigin != runnerOrigin {
		t.Fatalf("premise: runner dispatch lacks its committed origin: %+v", runnerEnvs)
	}
	engineOrigin := committedOriginRelation(t, "engine", runID, engineEvents)
	if len(engineEnvs) != 1 || engineEnvs[0].ChildWorkflowOrigin == nil || *engineEnvs[0].ChildWorkflowOrigin != engineOrigin {
		t.Fatalf("engine dispatch origin is not its committed origin: %+v", engineEnvs)
	}
	if runnerEnvs[0].Goober != engineEnvs[0].Goober || runnerEnvs[0].Attempt != engineEnvs[0].Attempt {
		t.Fatalf("child authority differs: runner goober=%q attempt=%d engine goober=%q attempt=%d",
			runnerEnvs[0].Goober, runnerEnvs[0].Attempt, engineEnvs[0].Goober, engineEnvs[0].Attempt)
	}
}
