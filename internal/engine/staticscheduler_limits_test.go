package engine

import (
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestStaticSchedulerBudgetCountsGateRepasses(t *testing.T) {
	in := schedulerInput("budget-repass", 1)
	in.Limit = 4 // seed + parallel + alpha + review; repass cannot dispatch.
	in.Run.Spec.Tasks[1].Next = "review"
	in.Run.Spec.Gates = []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "status-equals"}, Branches: map[string]string{"pass": wf.TargetJoin, "fail": "alpha"}}}
	writer, dir := newLiveWriter(t)
	exec := newScriptedExec(map[string][]scriptedCall{"seed": {succeed(map[string]interface{}{"value": "frozen-root"})}, "alpha": {fail("scripted_failure", "subject failed")}})
	out := executeScheduler(t, in, &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: exec, Auto: gate.NewAutomatedEvaluator()})
	if !strings.Contains(out.Error, "exceeded max steps (4)") || out.Result.Steps != 5 || exec.calls["alpha"] != 1 {
		t.Fatalf("steps=%d alpha visits=%d error=%s", out.Result.Steps, exec.calls["alpha"], out.Error)
	}
	assertSchedulerCompleteness(t, liveEvents(t, dir, in.Run.RunID), []journal.BranchStatus{journal.BranchFailed, journal.BranchCancelled, journal.BranchCancelled}, "")
}

func TestStaticSchedulerUnsupportedVisitAndBindingFailClosed(t *testing.T) {
	for _, mode := range []string{"repeat", "binding"} {
		t.Run(mode, func(t *testing.T) {
			in := schedulerInput("unsupported-"+mode, 1)
			want := "repeated static parallel visit"
			if mode == "repeat" {
				in.Run.Spec.Tasks[3].Next = "parallel"
			} else {
				in.Run.Spec.Tasks[1].ExpectedOutputs = []string{"value"}
				in.Run.Spec.Tasks[3].InputsFrom = map[string]string{"branchValue": "parallel.alpha.alpha.value"}
				want = "branch-qualified join input"
			}
			writer, dir := newLiveWriter(t)
			exec := newScriptedExec(map[string][]scriptedCall{"seed": {succeed(map[string]interface{}{"value": "frozen-root", "parallel.alpha.alpha.value": "must not bind a literal root key"})}})
			out := executeScheduler(t, in, &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: exec}, func(env *testsuite.TestWorkflowEnvironment) {
				if mode == "binding" {
					env.OnGetVersion(staticBranchInputsVersion, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
				}
			})
			if !strings.Contains(out.Error, want) {
				t.Fatalf("error=%s want=%s", out.Error, want)
			}
			visits := 0
			for _, event := range liveEvents(t, dir, in.Run.RunID) {
				if event.Type == journal.EventParallelStarted {
					visits++
				}
			}
			if mode == "binding" && (visits != 0 || len(exec.calls) != 1) {
				t.Fatalf("unsupported binding dispatched: visits=%d calls=%+v", visits, exec.calls)
			}
			if mode == "repeat" && visits != 1 {
				t.Fatalf("repeated visit admitted: %d", visits)
			}
		})
	}
}
