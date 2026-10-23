package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

type fanOutContractInput struct {
	Run  RunInput
	Mode string
}
type fanOutContractResult struct {
	FanOut fanOutResult
	Error  string
}

func fanOutContractWorkflow(ctx workflow.Context, input fanOutContractInput) (fanOutContractResult, error) {
	if workflow.GetVersion(ctx, "fanout-contract-v1", workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return fanOutContractResult{}, fmt.Errorf("legacy fan-out refusal")
	}
	in := input.Run
	m, err := wf.Compile(wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec}, wf.WithPreviewFeatures(true))
	if err != nil {
		return fanOutContractResult{}, err
	}
	r, err := newRunJournal(ctx, in, m)
	if err != nil {
		return fanOutContractResult{}, err
	}
	r.owner = &journalCommitOwner{root: r, mu: workflow.NewMutex(ctx)}
	r.stepBudget = &walkStepBudget{}
	r.runStarted(ctx)
	if err := r.emitPending(ctx); err != nil {
		return fanOutContractResult{}, err
	}
	seedResult := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: apiv1.IntegrityTrusted, Outputs: map[string]any{"value": "root"}}
	completed := newCompletedStages()
	completed.RecordCompleted(apiv1.Task{Name: "seed"}, seedResult)
	seed := &branchWalkFrame{upstream: map[string]apiv1.ResultEnvelope{"seed": seedResult}, evaluatedGates: map[string]bool{}, completed: completed, lastStage: "seed", lastResult: seedResult}
	plan := staticFanOutPlan(in.Spec.Parallels[0])
	if input.Mode == "skip" || input.Mode == "all-skip" {
		var selection journal.ParallelSelection
		activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
		if err := workflow.ExecuteActivity(activityCtx, freezeContractSelection, input.Mode).Get(ctx, &selection); err != nil {
			return fanOutContractResult{}, err
		}
		plan.Started.Selection = &selection
		for i := range plan.Branches {
			b := &plan.Branches[i]
			b.Optional = true
			if slices.Contains(selection.Selected, b.Name) {
				b.Reason = selection.Reasons[b.Name]
				continue
			}
			b.Reason = "not selected by frozen plan"
			b.PreSettled = &journal.BranchOutcome{Branch: b.ID, Name: b.Name, Status: journal.BranchSkipped, Reason: b.Reason}
		}
	}
	if input.Mode == "empty-map" {
		plan.Branches = nil
		plan.Started = journal.Event{Type: journal.EventMapStarted, MapRoster: &journal.MapRoster{Items: []journal.MapItem{}}}
		plan.Finished = journal.Event{Type: journal.EventMapFinished}
	}
	if input.Mode == "map-binding" {
		plan = contractMapPlan(plan)
	}
	if input.Mode == "binding" || input.Mode == "map-binding" || input.Mode == "integrity" {
		for i := range plan.Branches {
			identity := plan.Branches[i].Name
			plan.Branches[i].Bind = func(binding fanOutBindingContext) (fanOutBindings, error) {
				if binding.Started.Seq == 0 || binding.Started.Type != plan.Started.Type || binding.Started.Branch != 0 || binding.Branch != i+1 || binding.Stage != identity {
					return fanOutBindings{}, fmt.Errorf("body dispatched without committed visit context: %+v", binding)
				}
				return fanOutBindings{Values: map[string]any{"item": map[string]any{"identity": identity, "number": json.Number("9007199254740993")}, "visit": fmt.Sprint(binding.Started.Seq)}, Grades: map[string]apiv1.Integrity{"item": apiv1.IntegrityUnapproved, "visit": apiv1.IntegrityTrusted}}, nil
			}
		}
	}
	_, result, err := r.fanOut(ctx, in, m, plan, seed)
	out := fanOutContractResult{FanOut: result}
	if err != nil {
		out.Error = err.Error()
	}
	return out, nil
}

func executeFanOutContract(t *testing.T, in fanOutContractInput, acts *Activities) fanOutContractResult {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(acts)
	env.RegisterActivity(freezeContractSelection)
	env.ExecuteWorkflow(fanOutContractWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out fanOutContractResult
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFanOutDescriptorContract(t *testing.T) {
	for _, mode := range []string{"skip", "all-skip", "empty-map", "binding", "map-binding", "integrity"} {
		t.Run(mode, func(t *testing.T) {
			in := fanOutContractInput{Run: branchBodyInput("contract-"+mode, false, false).Run, Mode: mode}
			if mode == "integrity" {
				for i := range in.Run.Spec.Tasks {
					in.Run.Spec.Tasks[i].MinimumIntegrity = apiv1.IntegrityMaintainer
				}
			}
			w, dir := newLiveWriter(t)
			var calls atomic.Int32
			acts := &Activities{Journal: w, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				calls.Add(1)
				if mode == "binding" || mode == "map-binding" {
					assertDispatchedVisitIdentity(t, env, liveEvents(t, dir, in.Run.RunID))
					item := env.Inputs["item"].(map[string]any)
					if item["identity"] != stageOfTaskID(env.TaskID) {
						return apiv1.ResultEnvelope{}, fmt.Errorf("wrong branch item: %v", item)
					}
					// The in-memory binding clone itself is tested below; activity
					// transport may decode numbers according to its converter.
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "owned"}}, nil
			}}}
			out := executeFanOutContract(t, in, acts)
			if mode == "integrity" {
				if out.Error == "" || calls.Load() != 0 {
					t.Fatalf("unapproved binding admitted: %+v calls=%d", out, calls.Load())
				}
				return
			}
			if out.Error != "" || out.FanOut.Target != "join" {
				t.Fatalf("result: %+v", out)
			}
			want := int32(2)
			if mode == "skip" {
				want = 1
			}
			if mode == "all-skip" || mode == "empty-map" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d want=%d", calls.Load(), want)
			}
			events := liveEvents(t, dir, in.Run.RunID)
			for _, receipt := range []livejournal.AppendReceipt{out.FanOut.Started, out.FanOut.Finished} {
				if receipt.Seq == 0 || receipt.Seq > uint64(len(events)) || events[receipt.Seq-1].Type != receipt.Type {
					t.Fatalf("invalid lifecycle identity: %+v", receipt)
				}
			}
			for i, s := range out.FanOut.Settlements {
				if s.Outcome.Branch != i+1 {
					t.Fatalf("settlement order: %+v", out)
				}
				if (mode == "skip" && i == 1) || mode == "all-skip" {
					if s.Outcome.Status != journal.BranchSkipped || s.Outcome.Reason != "not selected by frozen plan" {
						t.Fatalf("skip changed: %+v", s)
					}
					for _, e := range events {
						if e.Branch == i+1 {
							t.Fatalf("skipped branch emitted activity: %+v", e)
						}
					}
				}
				if mode == "binding" || mode == "map-binding" {
					for _, result := range s.Results {
						if result.Integrity != apiv1.IntegrityUnapproved {
							t.Fatalf("binding grade lost: %+v", result)
						}
					}
				}
			}
		})
	}
}

func TestBranchBindingsDoNotShareMutableItems(t *testing.T) {
	values := map[string]any{"item": map[string]any{"number": json.Number("9007199254740993")}}
	r := &runJournal{branchWalk: &branchWalkFrame{bind: func(fanOutBindingContext) (fanOutBindings, error) {
		return fanOutBindings{Values: values, Grades: map[string]apiv1.Integrity{"item": apiv1.IntegrityMaintainer}}, nil
	}}}
	first, err := r.branchBindings(nil, "a")
	if err != nil {
		t.Fatal(err)
	}
	first.Values["item"].(map[string]any)["number"] = "mutated"
	second, err := r.branchBindings(nil, "b")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second.Values, values) {
		t.Fatal("binding mutated frozen source")
	}
}

type missingLifecycleReceipt struct{ JournalEmitter }

func (e missingLifecycleReceipt) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	resp, err := e.JournalEmitter.Emit(ctx, req)
	resp.AppendReceipts = nil
	return resp, err
}

func TestFanOutMissingLifecycleReceiptPreventsBranchDispatch(t *testing.T) {
	in := fanOutContractInput{Run: branchBodyInput("missing-lifecycle", false, false).Run}
	w, _ := newLiveWriter(t)
	var calls atomic.Int32
	acts := &Activities{Journal: missingLifecycleReceipt{w}, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		calls.Add(1)
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}}
	out := executeFanOutContract(t, in, acts)
	if !strings.Contains(out.Error, "missing lifecycle receipt") || calls.Load() != 0 || out.FanOut.Started.Seq != 0 {
		t.Fatalf("dispatch before receipt: %+v calls=%d", out, calls.Load())
	}
}

func freezeContractSelection(_ context.Context, mode string) (journal.ParallelSelection, error) {
	selected := []string{}
	if mode == "skip" {
		selected = append(selected, "alpha")
	}
	return journal.ParallelSelection{Selected: selected, Reasons: map[string]string{"alpha": "chosen"}}, nil
}

func TestFanOutSkippedBranchSurvivesCancellation(t *testing.T) {
	in := fanOutContractInput{Run: branchBodyInput("cancel-skipped", false, false).Run, Mode: "skip"}
	w, _ := newLiveWriter(t)
	acts := &Activities{Journal: w, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		<-ctx.Done()
		return apiv1.ResultEnvelope{}, ctx.Err()
	}}}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(acts)
	env.RegisterActivity(freezeContractSelection)
	started := 0
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == ActRunDeterministic {
			started++
			env.CancelWorkflow()
		}
	})
	env.ExecuteWorkflow(fanOutContractWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out fanOutContractResult
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if started != 1 || !strings.Contains(out.Error, "canceled") || len(out.FanOut.Settlements) != 2 {
		t.Fatalf("cancel: %+v starts=%d", out, started)
	}
	if out.FanOut.Settlements[0].Outcome.Status != journal.BranchCancelled || out.FanOut.Settlements[1].Outcome.Status != journal.BranchSkipped || out.FanOut.Settlements[1].Outcome.Reason != "not selected by frozen plan" {
		t.Fatalf("skip replaced: %+v", out)
	}
}

func TestFanOutSkippedBranchSurvivesFailFast(t *testing.T) {
	in := fanOutContractInput{Run: branchBodyInput("failed-skipped", false, false).Run, Mode: "skip"}
	in.Run.Spec.Parallels[0].FailurePolicy = apiv1.BranchFailFast
	in.Run.Spec.Parallels[0].OnFailure = wf.TargetEscalate
	w, _ := newLiveWriter(t)
	acts := &Activities{Journal: w, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultFailure}, nil
	}}}
	out := executeFanOutContract(t, in, acts)
	if out.Error != "" || out.FanOut.Target != wf.TargetEscalate || out.FanOut.Settlements[1].Outcome.Status != journal.BranchSkipped {
		t.Fatalf("fail-fast: %+v", out)
	}
}

type lostLifecycleAck struct {
	JournalEmitter
	lost atomic.Bool
}

func (e *lostLifecycleAck) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	resp, err := e.JournalEmitter.Emit(ctx, req)
	if err == nil && len(resp.AppendReceipts) > 0 && e.lost.CompareAndSwap(false, true) {
		return livejournal.EmitResponse{}, fmt.Errorf("lost lifecycle acknowledgement")
	}
	return resp, err
}

func TestFanOutLostAcknowledgementRetainsLifecycleIdentity(t *testing.T) {
	in := fanOutContractInput{Run: branchBodyInput("lost-lifecycle", false, false).Run, Mode: "all-skip"}
	w, dir := newLiveWriter(t)
	acts := &Activities{Journal: &lostLifecycleAck{JournalEmitter: w}, Workspaces: testWorkspaces(t)}
	out := executeFanOutContract(t, in, acts)
	if out.Error != "" {
		t.Fatalf("result: %+v", out)
	}
	starts := 0
	for _, event := range liveEvents(t, dir, in.Run.RunID) {
		if event.Type == journal.EventParallelStarted {
			starts++
			if event.Seq != out.FanOut.Started.Seq {
				t.Fatal("lifecycle identity changed")
			}
		}
	}
	if starts != 1 {
		t.Fatalf("duplicated start: %d", starts)
	}
}

func assertDispatchedVisitIdentity(t *testing.T, env apiv1.InvocationEnvelope, events []journal.Event) {
	t.Helper()
	for _, event := range events {
		if event.Type == journal.EventParallelStarted || event.Type == journal.EventMapStarted {
			if env.Inputs["visit"] != fmt.Sprint(event.Seq) {
				t.Errorf("dispatch visit = %v; committed start = %d", env.Inputs["visit"], event.Seq)
			}
			return
		}
	}
	t.Error("body dispatched before committed lifecycle start")
}

func contractMapPlan(plan fanOutPlan) fanOutPlan {
	digest := "sha256:" + strings.Repeat("a", 64)
	roster := &journal.MapRoster{Source: journal.FanOutSourceRef{Task: "seed", Slot: "items", Visit: 1, Attempt: 1, Digest: digest, Integrity: apiv1.IntegrityUnapproved},
		FailurePolicy: plan.FailurePolicy, MaxConcurrent: plan.MaxConcurrentBranches, Items: []journal.MapItem{}}
	for i := range plan.Branches {
		branch := &plan.Branches[i]
		branch.ItemIdentity, branch.ItemDigest = branch.Name, digest
		roster.Items = append(roster.Items, journal.MapItem{Identity: branch.Name, Digest: digest, Item: json.RawMessage(`{}`)})
	}
	plan.Started = journal.Event{Type: journal.EventMapStarted, MapRoster: roster}
	plan.Finished = journal.Event{Type: journal.EventMapFinished}
	return plan
}
