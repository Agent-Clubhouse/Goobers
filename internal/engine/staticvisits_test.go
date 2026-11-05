package engine

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

func repeatedVisitInput(name string, width int32) staticSchedulerInput {
	in := staticSchedulerInput{Run: branchBodyInput(name, false, false).Run}
	in.Run.Spec.Parallels[0].MaxConcurrentBranches = width
	in.Run.Spec.Tasks[1].ExpectedOutputs = []string{"value"}
	in.Run.Spec.Tasks[2].ExpectedOutputs = []string{"value"}
	in.Run.Spec.Tasks[3].InputsFrom = map[string]string{"alpha": "parallel.alpha.value", "beta": "parallel.beta.beta.value", "subject": "value"}
	in.Run.Spec.Tasks[3].Next = "again"
	in.Run.Spec.Gates = []apiv1.Gate{{Name: "again", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "status-equals"}, Branches: map[string]string{"pass": wf.TerminalComplete, "fail": "parallel"}}}
	return in
}

func TestConformanceRepeatedStaticVisitInputs(t *testing.T) {
	for _, width := range []int32{1, 2} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			in := repeatedVisitInput(fmt.Sprintf("repeat-inputs-%d", width), width)
			for _, lane := range []string{"local", "engine"} {
				var mu sync.Mutex
				counts := map[string]int{}
				var got []string
				det := &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
					mu.Lock()
					defer mu.Unlock()
					stage := stageOfTaskID(env.TaskID)
					counts[stage]++
					value := fmt.Sprintf("%s-%d", stage, counts[stage])
					grade := apiv1.IntegrityTrusted
					if stage == "alpha" && counts[stage] == 2 {
						grade = apiv1.IntegrityUnapproved
					}
					if stage == "join" {
						value = fmt.Sprintf("%v/%v/%v", env.Inputs["alpha"], env.Inputs["beta"], env.Inputs["subject"])
						got = append(got, value)
					}
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: grade, Outputs: map[string]any{"value": value}}, nil
				}}
				gates := 0
				auto := automatedFunc(func(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
					gates++
					if gates == 1 {
						return "fail", nil
					}
					return "pass", nil
				})
				var events []journal.Event
				if lane == "local" {
					events = runLocalFixture(t, conformanceFixture{spec: in.Run.Spec, dslVersion: "3.0", deterministic: det, automated: auto}, "local-"+in.Run.RunID)
				} else {
					writer, dir := newLiveWriter(t)
					out := executeScheduler(t, in, &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: det, Auto: auto})
					if out.Error != "" || out.Result.Status != StatusCompleted {
						t.Fatalf("result=%+v error=%s", out.Result, out.Error)
					}
					events = liveEvents(t, dir, in.Run.RunID)
				}
				want := []string{"alpha-1/beta-1/beta-1", "alpha-2/beta-2/beta-2"}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s join visit values=%v want=%v", lane, got, want)
				}
				assertRepeatedVisitJournal(t, lane, events, want)
			}
		})
	}
}

func assertRepeatedVisitJournal(t *testing.T, lane string, events []journal.Event, want []string) {
	t.Helper()
	visits, finishes, starts := 0, 0, 0
	var values []string
	var grades []apiv1.Integrity
	for _, event := range events {
		switch event.Type {
		case journal.EventParallelStarted:
			visits++
		case journal.EventParallelFinished:
			finishes++
		case journal.EventStageStarted:
			if event.Stage == "alpha" || event.Stage == "beta" {
				starts++
			}
		case journal.EventStageFinished:
			if event.Stage == "join" {
				values = append(values, fmt.Sprint(event.Outputs["value"]))
				grades = append(grades, event.Integrity)
			}
		}
	}
	if visits != 2 || finishes != 2 || starts != 4 {
		t.Fatalf("%s visit identity collapsed: starts=%d finishes=%d bodies=%d", lane, visits, finishes, starts)
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("%s journal values=%v", lane, values)
	}
	if !reflect.DeepEqual(grades, []apiv1.Integrity{apiv1.IntegrityTrusted, apiv1.IntegrityUnapproved}) {
		t.Fatalf("%s journal grades=%v", lane, grades)
	}
}

func TestStaticRepeatedVisitsKeepRunWideBudget(t *testing.T) {
	in := repeatedVisitInput("repeat-budget", 1)
	in.Limit = 7
	exec := newScriptedExec(map[string][]scriptedCall{"seed": {succeed(map[string]any{"value": "seed"})}, "alpha": {succeed(map[string]any{"value": "alpha"})}, "beta": {succeed(map[string]any{"value": "beta"})}})
	writer, dir := newLiveWriter(t)
	auto := automatedFunc(func(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
		return "fail", nil
	})
	out := executeScheduler(t, in, &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: exec, Auto: auto})
	if !strings.Contains(out.Error, "exceeded max steps (7)") || exec.calls["alpha"] != 1 || exec.calls["beta"] != 1 {
		t.Fatalf("budget reset across visit: error=%s calls=%v", out.Error, exec.calls)
	}
	visits := 0
	for _, event := range liveEvents(t, dir, in.Run.RunID) {
		if event.Type == journal.EventParallelStarted {
			visits++
		}
	}
	if visits != 2 {
		t.Fatalf("budget probe did not reach second visit: %d", visits)
	}
}

func TestStaticVisitMergeDoesNotRestoreForgottenSiblingOutput(t *testing.T) {
	old := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: apiv1.IntegrityTrusted, Outputs: map[string]any{"value": "old"}}
	fresh := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: apiv1.IntegrityUnapproved, Outputs: map[string]any{"value": "fresh"}}
	completed := newCompletedStages()
	completed.RecordCompleted(apiv1.Task{Name: "alpha"}, old)
	completed.RecordCompleted(apiv1.Task{Name: "beta"}, old)
	sibling := maps.Clone(completed)
	sibling.RecordCompleted(apiv1.Task{Name: "beta"}, fresh)
	seed := &branchWalkFrame{upstream: map[string]apiv1.ResultEnvelope{"alpha": old, "beta": old}, completed: completed, evaluatedGates: map[string]bool{}}
	walk := &staticParallelWalk{seed: seed, outcomes: []*staticBranchOutcome{
		{frame: &branchWalkFrame{visited: map[string]bool{"alpha": true}, completed: newCompletedStages(), upstream: map[string]apiv1.ResultEnvelope{"alpha": {Status: apiv1.ResultFailure}}}},
		{frame: &branchWalkFrame{visited: map[string]bool{"beta": true}, completed: sibling, upstream: map[string]apiv1.ResultEnvelope{"beta": fresh}, lastStage: "beta", lastResult: fresh}},
	}}
	walk.mergeVisit()
	if _, exists := seed.completed["alpha"]; exists {
		t.Fatal("sibling's seed restored an output forgotten by its owner")
	}
	value, found := resolveInputsFrom("beta.value", apiv1.ResultEnvelope{}, seed.completed, true)
	if !found || value != "fresh" || inputsFromIntegrity("beta.value", apiv1.ResultEnvelope{}, seed.completed, true) != apiv1.IntegrityUnapproved {
		t.Fatal("fresh sibling value/provenance lost")
	}
}
