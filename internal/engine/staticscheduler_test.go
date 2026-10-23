package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

const staticSchedulerVersion = "engine-static-scheduler-v1"

type staticSchedulerInput struct {
	Run   RunInput
	Limit int
}

type staticSchedulerResult struct {
	Result     RunResult
	Error      string
	Projection JournalProjection
}

// No production entry constructs a staticWalkSession or a shared budget.
// The version gate also preserves unmarked refusal histories in this fixture.
func staticScheduler(ctx workflow.Context, input staticSchedulerInput) (staticSchedulerResult, error) {
	if workflow.GetVersion(ctx, staticSchedulerVersion, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return staticSchedulerResult{}, fmt.Errorf("engine: static parallel execution is refused")
	}
	in := input.Run
	if !in.previewFeaturesEnabled() || !in.LiveJournal || len(in.Spec.Parallels) != 1 || len(in.Spec.Maps) != 0 {
		return staticSchedulerResult{}, fmt.Errorf("fixture requires preview, live journal and one static parallel")
	}
	if err := wf.RefuseChildWorkflowExecution(in.Spec); err != nil {
		return staticSchedulerResult{}, err
	}
	m, err := wf.Compile(wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec}, wf.WithPreviewFeatures(true))
	if err != nil {
		return staticSchedulerResult{}, err
	}
	rec, err := newRunJournal(ctx, in, m)
	if err != nil {
		return staticSchedulerResult{}, err
	}
	rec.owner = &journalCommitOwner{root: rec, mu: workflow.NewMutex(ctx)}
	rec.staticWalk = &staticWalkSession{seen: map[string]bool{}}
	rec.stepBudget = &walkStepBudget{limit: input.Limit}
	rec.runStarted(ctx)
	err = rec.emitPending(ctx)
	var result RunResult
	if err == nil {
		result, err = walk(ctx, in, m, rec, newHITLSession(in, m, rec))
	}
	out := staticSchedulerResult{Result: result}
	phase, _ := PhaseForStatus(result.Status)
	if err != nil {
		out.Error = err.Error()
		phase = journal.PhaseFailed
	}
	if ctx.Err() != nil {
		phase = journal.PhaseAborted
	}
	drain, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	rec.append(drain, journal.Event{Type: journal.EventRunFinished, Status: string(phase)})
	if err := rec.emitPending(drain); err != nil {
		out.Error += err.Error()
	}
	out.Projection = rec.projectionSnapshot()
	return out, nil
}

func schedulerInput(name string, width int32) staticSchedulerInput {
	in := branchBodyInput(name, false, false).Run
	gamma := detTask("gamma", "")
	gamma.Next = wf.TargetJoin
	gamma.InputsFrom = map[string]string{"root": "seed.value"}
	in.Spec.Tasks = append(in.Spec.Tasks, gamma)
	in.Spec.Parallels[0].Branches = append(in.Spec.Parallels[0].Branches, apiv1.Branch{Name: "gamma", Start: "gamma"})
	in.Spec.Parallels[0].MaxConcurrentBranches = width
	in.Spec.Tasks[3].Inputs = map[string]string{runsemantics.BranchCompletenessInput: "cannot override host completeness"}
	return staticSchedulerInput{Run: in}
}

func executeScheduler(t *testing.T, in staticSchedulerInput, acts *Activities, configure ...func(*testsuite.TestWorkflowEnvironment)) staticSchedulerResult {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(acts)
	for _, setup := range configure {
		setup(env)
	}
	env.ExecuteWorkflow(staticScheduler, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out staticSchedulerResult
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStaticSchedulerBoundedAdmissionAndJoin(t *testing.T) {
	for _, width := range []int32{1, 2, 3} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			in := schedulerInput(fmt.Sprintf("bounded-%d", width), width)
			writer, dir := newLiveWriter(t)
			var mu sync.Mutex
			active, peak, started := 0, 0, 0
			barrier := make(chan struct{})
			var completeness []journal.BranchOutcome
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				stage := stageOfTaskID(env.TaskID)
				if stage == "join" {
					data, err := json.Marshal(env.Inputs[runsemantics.BranchCompletenessInput])
					if err != nil {
						return apiv1.ResultEnvelope{}, err
					}
					if err := json.Unmarshal(data, &completeness); err != nil {
						return apiv1.ResultEnvelope{}, err
					}
				} else if stage != "seed" {
					assertCommittedBranchStart(t, liveEvents(t, dir, in.Run.RunID), stage)
					if env.Inputs["root"] != "frozen-root" {
						return apiv1.ResultEnvelope{}, fmt.Errorf("root seed missing for %s", stage)
					}
					mu.Lock()
					active++
					started++
					peak = max(peak, active)
					if started == int(width) {
						close(barrier)
					}
					mu.Unlock()
					select {
					case <-barrier:
					case <-ctx.Done():
						return apiv1.ResultEnvelope{}, ctx.Err()
					}
					mu.Lock()
					active--
					mu.Unlock()
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "frozen-root"}}, nil
			}}}
			out := executeScheduler(t, in, acts)
			if out.Error != "" || out.Result.Status != StatusCompleted || out.Result.Steps != 6 {
				t.Fatalf("result=%+v error=%s", out.Result, out.Error)
			}
			if peak != int(width) || started != 3 || active != 0 {
				t.Fatalf("peak=%d started=%d active=%d", peak, started, active)
			}
			assertSchedulerCompleteness(t, liveEvents(t, dir, in.Run.RunID), []journal.BranchStatus{journal.BranchSucceeded, journal.BranchSucceeded, journal.BranchSucceeded}, "join")
			if len(completeness) != 3 || completeness[0].Name != "alpha" || completeness[1].Name != "beta" || completeness[2].Name != "gamma" {
				t.Fatalf("join completeness = %+v", completeness)
			}
		})
	}
}

func assertCommittedBranchStart(t *testing.T, events []journal.Event, stage string) {
	t.Helper()
	for _, event := range events {
		if event.Type == journal.EventBranchStarted && event.Stage == stage {
			return
		}
	}
	t.Errorf("%s dispatched before durable branch start", stage)
}

func assertSchedulerCompleteness(t *testing.T, events []journal.Event, want []journal.BranchStatus, target string) {
	t.Helper()
	finished := map[int]bool{}
	for _, event := range events {
		if event.Type == journal.EventBranchFinished {
			if finished[event.Branch] {
				t.Errorf("duplicate branch finish %d", event.Branch)
			}
			finished[event.Branch] = true
		}
		if event.Type != journal.EventParallelFinished {
			continue
		}
		if len(finished) != len(want) || event.Target != target {
			t.Fatalf("parallel settled before branches or wrong target: %+v", event)
		}
		var got []journal.BranchStatus
		for i, outcome := range event.Completeness {
			if outcome.Branch != i+1 {
				t.Errorf("non-declaration order: %+v", event.Completeness)
			}
			got = append(got, outcome.Status)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("completeness=%v want=%v", got, want)
		}
		return
	}
	t.Fatal("parallel did not settle")
}

func TestConformanceStaticSchedulerFailurePoliciesAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		policy    apiv1.BranchFailurePolicy
		limit     int
		failure   bool
		want      []journal.BranchStatus
		target    string
		status    string
		errorText string
	}{
		{"continue", apiv1.BranchContinueOnError, 0, true, []journal.BranchStatus{journal.BranchFailed, journal.BranchSucceeded, journal.BranchSucceeded}, "join", StatusCompleted, ""},
		{"all-or-nothing", apiv1.BranchAllOrNothing, 0, true, []journal.BranchStatus{journal.BranchFailed, journal.BranchSucceeded, journal.BranchSucceeded}, wf.TargetAbort, StatusBlocked, ""},
		{"fail-fast", apiv1.BranchFailFast, 0, true, []journal.BranchStatus{journal.BranchFailed, journal.BranchCancelled, journal.BranchCancelled}, wf.TargetAbort, StatusBlocked, ""},
		{"budget", apiv1.BranchContinueOnError, 3, false, []journal.BranchStatus{journal.BranchSucceeded, journal.BranchFailed, journal.BranchCancelled}, "", "", "exceeded max steps (3)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := schedulerInput(tc.name, 1)
			in.Limit = tc.limit
			in.Run.Spec.Parallels[0].FailurePolicy = tc.policy
			if tc.policy != apiv1.BranchContinueOnError {
				in.Run.Spec.Parallels[0].OnFailure = wf.TargetAbort
			}
			alpha := succeed(map[string]interface{}{"value": "alpha"})
			if tc.failure {
				alpha = fail("scripted_failure", "failed")
			}
			writer, dir := newLiveWriter(t)
			script := map[string][]scriptedCall{"seed": {succeed(map[string]interface{}{"value": "frozen-root"})}, "alpha": {alpha}, "beta": {succeed(map[string]interface{}{"value": "beta"})}, "gamma": {succeed(map[string]interface{}{"value": "gamma"})}}
			// Production limits stay fixed. The small injected budget is an
			// engine-only stress case; each actual policy uses both runners.
			if tc.limit == 0 {
				local := runLocalFixture(t, conformanceFixture{spec: in.Run.Spec, script: script}, "local-scheduler-"+tc.name)
				assertSchedulerCompleteness(t, local, tc.want, tc.target)
			}
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Auto: gate.NewAutomatedEvaluator(), Det: newScriptedExec(script)}
			out := executeScheduler(t, in, acts)
			if out.Result.Status != tc.status || (tc.errorText == "" && out.Error != "") || !strings.Contains(out.Error, tc.errorText) {
				t.Fatalf("result=%+v error=%s", out.Result, out.Error)
			}
			if tc.limit > 0 && out.Result.Steps != tc.limit+1 {
				t.Fatalf("shared steps=%d", out.Result.Steps)
			}
			assertSchedulerCompleteness(t, liveEvents(t, dir, in.Run.RunID), tc.want, tc.target)
		})
	}
}
