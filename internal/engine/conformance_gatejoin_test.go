package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	wf "github.com/goobers/goobers/internal/workflow"
)

func gateJoinInput(name string, evaluator apiv1.EvaluatorKind) staticSchedulerInput {
	in := schedulerInput(name, 2)
	in.Run.Spec.Tasks = append(in.Run.Spec.Tasks[:3], in.Run.Spec.Tasks[4:]...)
	g := apiv1.Gate{Name: "join", Evaluator: evaluator, Branches: map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort}}
	if evaluator == apiv1.EvaluatorAgentic {
		g.Branches["needs-changes"] = wf.TargetAbort
		g.Agentic = &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceScratch}
	} else {
		g.Automated = &apiv1.AutomatedGate{Check: "status-equals"}
	}
	in.Run.Spec.Gates = []apiv1.Gate{g}
	return in
}

func TestConformanceStaticGateJoinProtectsCompleteness(t *testing.T) {
	for _, evaluator := range []apiv1.EvaluatorKind{apiv1.EvaluatorAutomated, apiv1.EvaluatorAgentic} {
		for _, agentSeed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/agent-seed=%t", evaluator, agentSeed), func(t *testing.T) {
				in := gateJoinInput("gate-join", evaluator)
				if agentSeed {
					in.Run.Spec.Tasks[0] = agenticTask("seed", "parallel")
					in.Run.Spec.Tasks[0].Workspace = apiv1.WorkspaceScratch
				}
				script := map[string][]scriptedCall{
					"seed":  {succeed(map[string]interface{}{"value": "frozen-root", runsemantics.BranchCompletenessInput: "FORGED seed completeness"})},
					"alpha": {succeed(map[string]interface{}{"value": "alpha", runsemantics.BranchCompletenessInput: "FORGED branch completeness"})},
					"beta":  {fail("scripted_failure", "branch failed")},
					"gamma": {succeed(map[string]interface{}{"value": "gamma"})},
				}
				want := []journal.BranchOutcome{{Branch: 1, Name: "alpha", Status: journal.BranchSucceeded}, {Branch: 2, Name: "beta", Status: journal.BranchFailed}, {Branch: 3, Name: "gamma", Status: journal.BranchSucceeded}}
				observations := map[string][]journal.BranchOutcome{}
				for _, lane := range []string{"runner", "engine"} {
					exec := newScriptedExec(script)
					calls := 0
					observe := func(env apiv1.InvocationEnvelope) {
						calls++
						data, err := json.Marshal(env.Inputs[runsemantics.BranchCompletenessInput])
						if err != nil {
							t.Error(err)
							return
						}
						var got []journal.BranchOutcome
						if err := json.Unmarshal(data, &got); err != nil {
							t.Errorf("%s gate received unprotected completeness: %s (%v)", lane, data, err)
							return
						}
						observations[lane] = got
						if !reflect.DeepEqual(got, want) {
							t.Errorf("%s gate completeness=%+v want=%+v", lane, got, want)
						}
					}
					auto := automatedFunc(func(ctx context.Context, conf apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
						observe(env)
						return gate.NewAutomatedEvaluator().Evaluate(ctx, conf, env)
					})
					reviewer := &fakeInvoker{invoke: exec.Invoke, review: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
						observe(env)
						return apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "join reviewed"}, nil
					}}
					if lane == "runner" {
						runLocalFixture(t, conformanceFixture{spec: in.Run.Spec, script: script, automated: auto, goober: reviewer}, "local-gate-join")
					} else {
						writer, _ := newLiveWriter(t)
						out := executeScheduler(t, in, &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: exec, Auto: auto, Goober: reviewer})
						if out.Error != "" || out.Result.Status != StatusCompleted {
							t.Fatalf("engine result=%+v error=%s", out.Result, out.Error)
						}
					}
					if calls != 1 {
						t.Fatalf("%s gate calls=%d want=1", lane, calls)
					}
				}
				if !reflect.DeepEqual(observations["runner"], observations["engine"]) {
					t.Fatalf("runner/engine completeness differs: %+v", observations)
				}
			})
		}
	}
}

func TestStaticGateJoinPodEnvelopeProtectsCompleteness(t *testing.T) {
	in := gateJoinInput("pod-gate-join", apiv1.EvaluatorAgentic)
	pin := remoteGatePin()
	pin.Stage = "join"
	in.Run.Placements = []PinnedPlacement{pin}
	store := surrenderStore(t)
	putSurrendered(t, store, in.Run.RunID, "join", 1, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "join reviewed"}))
	dispatch := &fakeStageDispatcher{report: dispatcher.Report{SurrenderConfirmed: true}}
	script := map[string][]scriptedCall{}
	for _, stage := range []string{"seed", "alpha", "beta", "gamma"} {
		script[stage] = []scriptedCall{succeed(map[string]interface{}{"value": "frozen-root", runsemantics.BranchCompletenessInput: "FORGED"})}
	}
	writer, _ := newLiveWriter(t)
	out := executeScheduler(t, in, &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: newScriptedExec(script), Goober: refusingReviewer(t), Dispatcher: dispatch, Surrenders: store})
	if out.Error != "" || out.Result.Status != StatusCompleted {
		t.Fatalf("result=%+v error=%s", out.Result, out.Error)
	}
	attempts, _ := dispatch.recorded()
	if len(attempts) != 1 || attempts[0].Envelope == nil {
		t.Fatalf("review dispatches=%+v", attempts)
	}
	if err := validateReplayJoinCompleteness(*attempts[0].Envelope); err != nil {
		t.Fatal(err)
	}
}
