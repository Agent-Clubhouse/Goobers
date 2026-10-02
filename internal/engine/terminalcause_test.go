package engine

import (
	"errors"
	"reflect"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestEngineTerminalCauseRetryAndInfrastructure(t *testing.T) {
	for _, infra := range []bool{false, true} {
		t.Run(map[bool]string{false: "policy", true: "infra"}[infra], func(t *testing.T) {
			failure := errors.New("dispatch unavailable")
			class := journal.TerminalRetryExhaustion
			if infra {
				failure = invoke.InfrastructureFailure(failure)
				class = journal.TerminalInfrastructureFailure
			}
			proj := executeForProjection(t, runInput("engine-terminal-cause", retrySpec(&apiv1.RetryPolicy{MaxAttempts: 2})), &Activities{Det: &scriptedDeterministic{failures: persistentFailures(failure, 100)}, Workspaces: testWorkspaces(t)}, true)
			c := proj.Ops[len(proj.Ops)-1].Event.TerminalCause
			if c == nil || c.Classification != class || c.Selector != "implement" || c.Retry == nil || c.Retry.Consumed != c.Retry.Allowed || c.CausalEventSeq != uint64(len(proj.Ops)-1) {
				t.Fatalf("cause=%+v", c)
			}
			dir, err := ProjectRun(t.TempDir(), proj)
			if err != nil {
				t.Fatal(err)
			}
			rd, err := journal.OpenRead(dir)
			if err != nil {
				t.Fatal(err)
			}
			durable, err := rd.TerminalCause()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(durable, c) {
				t.Fatalf("durable=%+v expected=%+v", durable, c)
			}
		})
	}
}

func TestEngineTerminalCauseVersionPreservesOldHistories(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "legacy"}[legacy], func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			if legacy {
				env.OnGetVersion(terminalCauseChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			env.RegisterActivity(&Activities{Det: &scriptedDeterministic{failures: persistentFailures(errors.New("dispatch unavailable"), 100)}, Workspaces: testWorkspaces(t)})
			env.ExecuteWorkflow(Run, runInput("cause-version", retrySpec(nil)))
			if env.GetWorkflowError() == nil {
				t.Fatal("expected failure")
			}
			value, err := env.QueryWorkflow(JournalQuery)
			if err != nil {
				t.Fatal(err)
			}
			var proj JournalProjection
			if err := value.Get(&proj); err != nil {
				t.Fatal(err)
			}
			cause := proj.Ops[len(proj.Ops)-1].Event.TerminalCause
			if (cause == nil) != legacy {
				t.Fatalf("legacy=%v cause=%+v", legacy, cause)
			}
		})
	}
}

func TestEngineTerminalCauseGateTargets(t *testing.T) {
	for _, target := range []string{journal.TargetAbort, journal.TargetEscalate, "settle"} {
		t.Run(target, func(t *testing.T) {
			tasks := []apiv1.Task{crTask("implement", "review")}
			if target == "settle" {
				tasks = append(tasks, crTask("settle", journal.TargetEscalate))
			}
			spec := crSpec("implement", tasks, []apiv1.Gate{crGate("review", map[string]string{"pass": "", "fail": target})})
			proj := executeForProjection(t, runInput("gate-cause", spec), &Activities{Det: &scriptedStages{results: map[string][]apiv1.ResultEnvelope{"implement": {failureResult("WORK_FAILED", "work failed")}}}, Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t)}, false)
			c := proj.Ops[len(proj.Ops)-1].Event.TerminalCause
			if c == nil || c.SelectorKind != "gate" || c.Selector != "review" || c.Target != target || c.Verdict != "fail" || c.CausalEventSeq == 0 {
				t.Fatalf("cause=%+v", c)
			}
			if proj.Ops[c.CausalEventSeq-1].Event.Type != journal.EventGateEvaluated {
				t.Fatalf("causal sequence points at wrong event: %+v", c)
			}
		})
	}
}
