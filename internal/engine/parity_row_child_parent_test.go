package engine

// Parity row ECW-child-parent (HA-P4-09): a parent stage that yields to one
// generated child and continues.
//
// Runner: invokeWithChildHandoff observes the host's accepted request, journals
// child.workflow.wait, waits on the host, journals child.workflow.continued and
// starts the next attempt of the same occurrence (run.go's yield arm: the
// attempt ID advances, no retry allowance is consumed). Engine: the same stage
// admits, parks, executes the child as a Temporal child workflow of Run and
// continues through errChildContinued.
//
// The row compares the parent journal's lifecycle shape and the identity
// relations between its starts. It does not compare child internals: the
// runner's host owns its child run, while the engine's is a separate Temporal
// execution with its own journal. Production Run still refuses child parents
// (#7758), so the engine side uses the protocol entry.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// oneChildHandoff yields the first parent invocation to one accepted child
// and reports it completed; later invocations observe no request.
type oneChildHandoff struct {
	mu        sync.Mutex
	requested bool
}

func (h *oneChildHandoff) Await(ctx context.Context, env apiv1.InvocationEnvelope) (runner.ChildHandoffRequest, error) {
	h.mu.Lock()
	first := !h.requested && env.ChildWorkflowOrigin != nil
	h.requested = true
	h.mu.Unlock()
	if !first {
		<-ctx.Done()
		return runner.ChildHandoffRequest{}, ctx.Err()
	}
	childID := env.RunID + "-child-1"
	request := journal.ChildHandoffRequest{Gaggle: env.Gaggle, ParentRunID: env.RunID, RequestID: journal.Digest([]byte(childID)), Action: "wait", ChildRunID: childID,
		AcceptanceID: "trigger-" + childID, InvocationKey: childID, SourceDigest: journal.Digest([]byte("proposal")), Origin: *env.ChildWorkflowOrigin}
	return runner.ChildHandoffRequest(request), nil
}
func (*oneChildHandoff) Yield(context.Context, runner.ChildHandoffRequest, runner.ChildWorkspaceCustody) error {
	return nil
}
func (*oneChildHandoff) Wait(context.Context, runner.ChildHandoffRequest) (runner.ChildHandoffCompletion, error) {
	return runner.ChildHandoffCompletion{State: "completed", Summary: "bounded child summary", ResultRef: journal.Digest([]byte("result"))}, nil
}

// quiescentGoober registers and settles one workspace writer per invocation,
// the stop/join evidence a real process owner supplies before a yield.
type quiescentGoober struct{ invoke.Goober }

func (g quiescentGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if done := invoke.RegisterWorkspaceWriter(ctx); done != nil {
		defer done(nil)
	}
	return g.Goober.Invoke(ctx, env)
}

type resumingCapacity struct{}

func (resumingCapacity) SuspendChildParent(context.Context, string) (runner.ChildParentSuspension, error) {
	return resumingCapacity{}, nil
}
func (resumingCapacity) Resume(context.Context) error { return nil }

func childParentRunnerSide(t *testing.T, runID string) ([]journal.Event, []apiv1.InvocationEnvelope, error) {
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
			return quiescentGoober{exec.boundTo(goober)}, nil
		},
		Automated:            gate.NewAutomatedEvaluator(),
		Worktrees:            workcopies,
		RunsDir:              filepath.Join(root, "runs"),
		ScratchDir:           filepath.Join(root, "scratch"),
		RepoCloneURL:         func(apiv1.RepoRef) (string, error) { return repo, nil },
		AdmitParentExecution: func(context.Context, *wf.Machine) error { return nil },
		ChildHandoff:         &oneChildHandoff{},
		ChildParentCapacity:  resumingCapacity{},
		BorrowParentJournal:  func(string, string, *journal.Run) (func(), error) { return func() {}, nil },
		RestoreParentArchive: func(context.Context, runner.OwnedJournalRecorder, runner.ParentWorkspaceArchive, uint64) error {
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := wf.Compile(wf.Definition{Name: "child-parent", Version: 1, DSLVersion: "3.1", Spec: childOriginParitySpec()}, wf.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := r.Start(context.Background(), runner.StartInput{RunID: runID, Machine: machine, Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: parityRepoRef})
	exec.mu.Lock()
	defer exec.mu.Unlock()
	return readJournalEvents(t, filepath.Join(root, "runs", runID)), append([]apiv1.InvocationEnvelope(nil), exec.envs...), runErr
}

func childParentEngineSide(t *testing.T, runID string) ([]journal.Event, []apiv1.InvocationEnvelope, error) {
	t.Helper()
	f := newChildFixture()
	env, dir := childEnvironment(t, f)
	in := runInput("child-parent", childOriginParitySpec())
	in.RunID, in.DSLVersion, in.LiveJournal, in.RepoRef = runID, "3.1", true, parityRepoRef
	env.ExecuteWorkflow("Run", in)
	return liveEvents(t, dir, runID), f.envelopes, env.GetWorkflowError()
}

// parentLifecycle is the parent stage's shape: starts, child lifecycle
// annotations and finishes, by attempt, in journal order.
func parentLifecycle(events []journal.Event) []string {
	var out []string
	for _, event := range events {
		if event.Stage != "parent" {
			continue
		}
		switch {
		case event.Type == journal.EventStageStarted, event.Type == journal.EventStageFinished:
			out = append(out, fmt.Sprintf("%s#%d", event.Type, event.Attempt))
		case event.Runner["kind"] == journal.ChildWaitKind, event.Runner["kind"] == journal.ChildContinuedKind:
			out = append(out, fmt.Sprintf("%s#%d", event.Runner["kind"], event.Attempt))
		}
	}
	return out
}

// continuationRelation checks that every start after the first continues the
// first start's occurrence with an identity derived from its own sequence.
func continuationRelation(t *testing.T, side, runID string, events []journal.Event) {
	t.Helper()
	var occurrence string
	for _, event := range events {
		if event.Type != journal.EventStageStarted || event.Stage != "parent" {
			continue
		}
		origin, err := journal.ChildWorkflowOriginForEvent(runID, event)
		if err != nil {
			t.Fatalf("%s: start #%d is unbound: %v", side, event.Attempt, err)
		}
		if origin.AttemptID != journal.StageAttemptID(runID, 0, "parent", event.Seq) {
			t.Fatalf("%s: start #%d attempt identity is not its committed sequence", side, event.Attempt)
		}
		if occurrence == "" {
			occurrence = origin.StageOccurrence
		} else if origin.StageOccurrence != occurrence {
			t.Fatalf("%s: start #%d left the occurrence", side, event.Attempt)
		}
	}
}

func TestParityRowChildParentWithOneGeneratedChild(t *testing.T) {
	const runID = "run-child-parent-row"
	runnerEvents, runnerEnvs, runnerErr := childParentRunnerSide(t, runID)
	engineEvents, engineEnvs, engineErr := childParentEngineSide(t, runID)
	if runnerErr != nil || engineErr != nil {
		t.Fatalf("walk outcome differs or failed: runner=%v engine=%v", runnerErr, engineErr)
	}
	want := []string{"stage.started#1", journal.ChildWaitKind + "#1", journal.ChildContinuedKind + "#1", "stage.started#2", "stage.finished#2"}
	// Premise: the runner really yields once and continues the occurrence.
	if got := parentLifecycle(runnerEvents); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("premise: runner parent lifecycle = %v, want %v", got, want)
	}
	continuationRelation(t, "runner", runID, runnerEvents)
	if got := parentLifecycle(engineEvents); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("engine parent lifecycle = %v, runner = %v", got, want)
	}
	continuationRelation(t, "engine", runID, engineEvents)
	if len(runnerEnvs) != 2 || len(engineEnvs) != 2 {
		t.Fatalf("dispatch counts differ: runner=%d engine=%d", len(runnerEnvs), len(engineEnvs))
	}
	for i := range runnerEnvs {
		r, e := runnerEnvs[i], engineEnvs[i]
		if r.Attempt != e.Attempt || r.Goober != e.Goober || (r.ChildWorkflowOrigin == nil) != (e.ChildWorkflowOrigin == nil) {
			t.Fatalf("dispatch %d authority differs: runner attempt=%d goober=%q engine attempt=%d goober=%q", i, r.Attempt, r.Goober, e.Attempt, e.Goober)
		}
	}
	// The continued attempt carries exactly one more context pointer on both
	// sides: the child's bounded, untrusted return.
	runnerAdded := len(runnerEnvs[1].ContextPointers) - len(runnerEnvs[0].ContextPointers)
	engineAdded := len(engineEnvs[1].ContextPointers) - len(engineEnvs[0].ContextPointers)
	if runnerAdded != 1 || engineAdded != 1 {
		t.Fatalf("continuation context differs: runner added %d, engine added %d", runnerAdded, engineAdded)
	}
	if got := engineEnvs[1].ContextPointers[len(engineEnvs[1].ContextPointers)-1].Integrity; got != runnerEnvs[1].ContextPointers[len(runnerEnvs[1].ContextPointers)-1].Integrity {
		t.Fatalf("child return grade differs: engine %q runner %q", got, runnerEnvs[1].ContextPointers[len(runnerEnvs[1].ContextPointers)-1].Integrity)
	}
}
