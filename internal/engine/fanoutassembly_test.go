package engine

import (
	"context"
	"errors"
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
				want = `unknown state "parallel"`
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
			if mode == "legacy" {
				if calls != 1 {
					t.Fatalf("legacy stage dispatches=%d want seed only", calls)
				}
				for _, event := range liveEvents(t, dir, in.RunID) {
					if event.Type == journal.EventParallelStarted {
						t.Fatal("legacy history entered new controller")
					}
				}
			} else if calls != 0 {
				t.Fatalf("rejected input dispatched %d stages", calls)
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
