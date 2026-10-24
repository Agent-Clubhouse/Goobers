package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// These exercise the registered production workflow, including its terminal
// and repair-query lifecycle, rather than the scheduler-only test assembly.
func TestProductionRunFanOutTerminal(t *testing.T) {
	for _, failBranch := range []bool{false, true} {
		name, wantStatus, wantPhase := "success", StatusCompleted, journal.PhaseCompleted
		if failBranch {
			name, wantStatus, wantPhase = "failed", StatusBlocked, journal.PhaseAborted
		}
		t.Run(name, func(t *testing.T) {
			in := branchBodyInput("production-"+name, false, false).Run
			in.Spec.Parallels[0].FailurePolicy = apiv1.BranchAllOrNothing
			in.Spec.Parallels[0].OnFailure = wf.TargetAbort
			writer, dir := newLiveWriter(t)
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				stage := stageOfTaskID(env.TaskID)
				if stage == "alpha" || stage == "beta" {
					assertCommittedBranchStart(t, liveEvents(t, dir, in.RunID), stage)
				}
				res := recoveryResult(stage)
				if failBranch && stage == "alpha" {
					res.Status = apiv1.ResultFailure
				}
				return res, nil
			}}}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(acts)
			env.ExecuteWorkflow(Run, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var out RunResult
			if err := env.GetWorkflowResult(&out); err != nil {
				t.Fatal(err)
			}
			wantSteps := 5 // seed, parallel, alpha, beta, join
			if failBranch {
				wantSteps = 4
			}
			if out.Status != wantStatus || out.Steps != wantSteps {
				t.Fatalf("result=%+v want status=%s steps=%d", out, wantStatus, wantSteps)
			}
			proj := productionFanOutProjection(t, env)
			assertProductionFanOutTerminal(t, proj, wantPhase)
			events := liveEvents(t, dir, in.RunID)
			if events[len(events)-1].Type != journal.EventRunFinished || events[len(events)-1].Status != string(wantPhase) {
				t.Fatalf("live terminal=%+v", events[len(events)-1])
			}
			statuses := []journal.BranchStatus{journal.BranchSucceeded, journal.BranchSucceeded}
			join := "join"
			if failBranch {
				statuses[0], join = journal.BranchFailed, wf.TargetAbort
			}
			assertSchedulerCompleteness(t, events, statuses, join)
		})
	}
}

func TestProductionRunFanOutCancellationDrainsBeforeTerminal(t *testing.T) {
	in := branchBodyInput("production-cancel", false, false).Run
	writer, dir := newLiveWriter(t)
	acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		if stageOfTaskID(env.TaskID) == "seed" {
			return recoveryResult("seed"), nil
		}
		<-ctx.Done()
		return apiv1.ResultEnvelope{}, ctx.Err()
	}}}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(acts)
	started := 0
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		if info.ActivityType.Name != ActRunDeterministic {
			return
		}
		var inv apiv1.InvocationEnvelope
		if err := args.Get(&inv); err != nil {
			t.Fatal(err)
		}
		if stageOfTaskID(inv.TaskID) == "seed" {
			return
		}
		started++
		if started == 2 {
			env.CancelWorkflow()
		}
	})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancellation error=%v", err)
	}
	if started != 2 {
		t.Fatalf("started branches=%d", started)
	}
	proj := productionFanOutProjection(t, env)
	assertProductionFanOutTerminal(t, proj, journal.PhaseAborted)
	events := liveEvents(t, dir, in.RunID)
	assertSchedulerCompleteness(t, events, []journal.BranchStatus{journal.BranchCancelled, journal.BranchCancelled}, "")
	if events[len(events)-1].Status != string(journal.PhaseAborted) {
		t.Fatalf("terminal=%+v", events[len(events)-1])
	}
}

func TestProductionRunFanOutLegacyAndAdmission(t *testing.T) {
	for _, mode := range []string{"legacy", "no-live", "no-preview"} {
		t.Run(mode, func(t *testing.T) {
			in := branchBodyInput("production-"+mode, false, false).Run
			writer, dir := newLiveWriter(t)
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			want := "fan-out requires preview features and live journaling"
			switch mode {
			case "legacy":
				env.OnGetVersion(fanOutAssemblyVersion, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
				want = wf.ErrParallelsUnsupported.Error()
			case "no-live":
				in.LiveJournal = false
			case "no-preview":
				enabled := false
				in.PreviewFeaturesEnabled = &enabled
				want = "preview"
			}
			calls := 0
			env.RegisterActivity(&Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				calls++
				return recoveryResult("seed"), nil
			}}})
			env.ExecuteWorkflow(Run, in)
			if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error=%v want=%q", err, want)
			}
			if calls != 0 {
				t.Fatalf("rejected input dispatched %d stages", calls)
			}
			if _, err := os.Stat(filepath.Join(dir, in.RunID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("run-start refusal created a journal: %v", err)
			}
		})
	}
}

func productionFanOutProjection(t *testing.T, env *testsuite.TestWorkflowEnvironment) JournalProjection {
	t.Helper()
	value, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatal(err)
	}
	var proj JournalProjection
	if err := value.Get(&proj); err != nil {
		t.Fatal(err)
	}
	return proj
}

func assertProductionFanOutTerminal(t *testing.T, proj JournalProjection, phase journal.RunPhase) {
	t.Helper()
	finished, settled := 0, 0
	for _, op := range proj.Ops {
		if op.Event == nil {
			continue
		}
		if op.Event.Type == journal.EventBranchFinished {
			settled++
		}
		if op.Event.Type == journal.EventRunFinished {
			finished++
			if settled != 2 || op.Event.Status != string(phase) {
				t.Fatalf("terminal before branch drain: settled=%d event=%+v", settled, op.Event)
			}
		}
	}
	if finished != 1 {
		t.Fatalf("terminal count=%d", finished)
	}
}

func TestProductionRunFanOutRetainsUndeliveredTerminal(t *testing.T) {
	in := branchBodyInput("production-terminal-repair", false, false).Run
	writer, dir := newLiveWriter(t)
	attempts := 0
	emitter := receiptCleanupEmitter(func(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
		for _, op := range req.Ops {
			if op.Event != nil && op.Event.Type == journal.EventRunFinished {
				attempts++
				return livejournal.EmitResponse{}, errors.New("terminal delivery unavailable")
			}
		}
		return writer.Emit(ctx, req)
	})
	acts := &Activities{Journal: emitter, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return recoveryResult(stageOfTaskID(env.TaskID)), nil
	}}}
	proj := executeLive(t, in, acts, false)
	assertProductionFanOutTerminal(t, proj, journal.PhaseCompleted)
	if attempts != emitMaxAttempts {
		t.Fatalf("terminal delivery attempts=%d want=%d", attempts, emitMaxAttempts)
	}
	for _, event := range liveEvents(t, dir, in.RunID) {
		if event.Type == journal.EventRunFinished {
			t.Fatal("unacknowledged terminal reached live journal")
		}
	}
}

func TestProductionRunFanOutComposesWithSelection(t *testing.T) {
	in, acts, calls := selectiveParallelFixture(t, `{"schemaVersion":1,"branches":[{"name":"gamma","reason":"affected"}]}`)
	proj := executeLive(t, in.Run, acts, false)
	got := calls.snapshot()
	if got["alpha"] != 1 || got["beta"] != 0 || got["gamma"] != 1 || got["join"] != 1 {
		t.Fatalf("Run did not use frozen selection: %v", got)
	}
	for _, op := range proj.Ops {
		event := op.Event
		if event == nil || event.Type != journal.EventParallelFinished {
			continue
		}
		outcomes := event.Completeness
		if event.Target != "join" || len(outcomes) != 3 || outcomes[0].Status != journal.BranchSucceeded || outcomes[1].Status != journal.BranchSkipped || outcomes[2].Status != journal.BranchSucceeded || outcomes[1].Reason != "not selected by seed.selection" {
			t.Fatalf("Run selection settlement=%+v", event)
		}
		return
	}
	t.Fatal("Run did not settle its selective parallel")
}

func TestProductionRunLegacyMapHistoryKeepsRefusal(t *testing.T) {
	for _, withParallel := range []bool{false, true} {
		def := fanOutEngineDefinition(true)
		if withParallel {
			def.Spec.Parallels = branchBodyInput("unused", false, false).Run.Spec.Parallels
		}
		in := runInput("map-still-refused", def.Spec)
		in.DSLVersion, in.LiveJournal = "3.1", true
		var suite testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&suite)
		if withParallel {
			env.OnGetVersion(fanOutAssemblyVersion, workflow.DefaultVersion, 1).
				Return(workflow.DefaultVersion)
		} else {
			env.OnGetVersion(staticMapVersion, workflow.DefaultVersion, 1).
				Return(workflow.DefaultVersion)
		}
		// No activities registered: any attempt to run the producer is a failure.
		env.ExecuteWorkflow(Run, in)
		want := wf.ErrMapUnsupported
		if withParallel {
			want = wf.ErrParallelsUnsupported
		}
		if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), want.Error()) {
			t.Fatalf("map with parallel=%v: refusal=%v, want %v", withParallel, err, want)
		}
	}
}
