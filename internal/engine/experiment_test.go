package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// This qualification entry exercises the real walk while the production
// admission refusal remains in force. Remove it when qualification permits Run.
func experimentQualification(ctx workflow.Context, in RunInput) (RunResult, error) {
	m, err := wf.Compile(wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec}, wf.WithPreviewFeatures(true))
	if err != nil {
		return RunResult{}, err
	}
	rec, err := newRunJournal(ctx, in, m)
	if err != nil {
		return RunResult{}, err
	}
	rec.runStarted(ctx)
	result, err := walk(ctx, in, m, rec, newHITLSession(in, m, rec))
	phase, _ := PhaseForStatus(result.Status)
	if err != nil {
		phase = journal.PhaseFailed
	}
	if finishErr := rec.runFinished(ctx, phase, journal.RunDispositionProduced, result.FinalState); finishErr != nil {
		return result, finishErr
	}
	return result, err
}

func experimentParityCase(seed uint64) parityCase {
	task := detTask("build", "review")
	task.Inputs = map[string]string{"choice": "baseline"}
	task.Experiment = &apiv1.BanditExperiment{
		Seed: seed, Arms: []apiv1.BanditArm{
			{Name: "control", GateLevel: 1, Variant: map[string]string{"choice": "control"}},
			{Name: "treatment", GateLevel: 1, Variant: map[string]string{"choice": "treatment"}},
		}, ExplorationBudget: 100, MinSamples: 2, MaxFailureRate: .75, MinLift: .1, Confidence: .8, TrainWindow: 4, EvalWindow: 4, DefaultGateLevel: 1,
	}
	return parityCase{Row: "P4-experiment", DSLVersion: "2.0", Spec: fixtureSpec("build", []apiv1.Task{task}, []apiv1.Gate{statusGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})}), Script: map[string][]scriptedCall{"build": {succeed(nil)}}}
}

func runExperimentQualification(t *testing.T, c parityCase, runID string, plane bandit.ObservationPlane, remote bool) paritySide {
	t.Helper()
	exec := newRecordingExecForCase(c)
	in := parityEngineRunInput(t, c, runID)
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.SetStartTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	env.RegisterActivity(&Activities{Bandit: plane, Det: exec, Goober: exec, Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t)})
	if remote {
		in.Placements = []PinnedPlacement{remotePin("build")}
		in.SelfExecutionDenied = len(c.Spec.Tasks) == 1
		env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(ctx context.Context, input DispatchStageInput) (stageActivityResult, error) {
			result, err := exec.Run(ctx, input.Envelope, *input.Run)
			return stageActivityResult{ResultEnvelope: result}, err
		})
	}
	env.ExecuteWorkflow(experimentQualification, in)
	side := paritySide{Name: "engine", Err: env.GetWorkflowError(), Envelopes: exec.projected(), Events: projectEngineJournal(t, env)}
	if side.Err == nil {
		var result RunResult
		if err := env.GetWorkflowResult(&result); err != nil {
			t.Fatal(err)
		}
		side.Terminal = parityTerminal{Status: result.Status, FinalState: result.FinalState, FailureCode: result.FailureCode, NoWork: result.NoWork}
	}
	return side
}

func experimentJournalDecisions(t *testing.T, events []journal.Event) []string {
	t.Helper()
	var decisions []string
	for _, event := range events {
		switch event.Type {
		case journal.EventBanditAssignment, journal.EventBanditObservation, journal.EventBanditRetired, journal.EventBanditPromotionProposed:
			raw, err := json.Marshal(event.Outputs)
			if err != nil {
				t.Fatal(err)
			}
			decisions = append(decisions, string(event.Type)+":"+string(raw))
		}
	}
	return decisions
}

func TestExperimentWalkMatchesRunnerFixedSeeds(t *testing.T) {
	arms := map[string]bool{}
	for seed := uint64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			c := experimentParityCase(seed)
			c.BanditHistory = experimentSampleFloor()
			runID := fmt.Sprintf("experiment-%d", seed)
			local := runParityRunnerSide(t, c, runID)
			plane := &experimentTestPlane{history: slices.Clone(c.BanditHistory)}
			cloud := runExperimentQualification(t, c, runID, plane, false)
			assertParityHarnessIsNotVacuous(t, local)
			assertParityHarnessIsNotVacuous(t, cloud)
			if err := checkAllSurfaces(parityObservation{Case: c, Runner: local, Engine: cloud}); err != nil {
				t.Fatal(err)
			}
			if got, want := experimentJournalDecisions(t, cloud.Events), experimentJournalDecisions(t, local.Events); !reflect.DeepEqual(got, want) {
				t.Fatalf("cloud decisions %v; local %v", got, want)
			}
			if plane.reads != 1 || len(plane.writes) != 1 {
				t.Fatalf("reads=%d writes=%v", plane.reads, plane.writes)
			}
			arms[plane.writes[0].Observation.Arm] = true
		})
	}
	if len(arms) != 2 {
		t.Fatalf("fixed-seed cohort did not exercise both arms: %v", arms)
	}
}

func TestExperimentRemoteDispatchUsesAssignedInputs(t *testing.T) {
	c := experimentParityCase(7)
	local := runParityRunnerSide(t, c, "experiment-remote")
	plane := &experimentTestPlane{history: slices.Clone(c.BanditHistory)}
	cloud := runExperimentQualification(t, c, "experiment-remote", plane, true)
	if err := diffParityEnvelopes(parityObservation{Case: c, Runner: local, Engine: cloud}); err != nil {
		t.Fatal(err)
	}
	if cloud.Err != nil || len(plane.writes) != 1 {
		t.Fatalf("cloud error %v; writes %v", cloud.Err, plane.writes)
	}
	if got, want := experimentJournalDecisions(t, cloud.Events), experimentJournalDecisions(t, local.Events); !reflect.DeepEqual(got, want) {
		t.Fatalf("cloud decisions %v; local %v", got, want)
	}
}

func experimentSampleFloor() []bandit.Observation {
	var observations []bandit.Observation
	for _, arm := range []string{"control", "treatment"} {
		for i := range 2 {
			observations = append(observations, bandit.Observation{Schema: bandit.Schema, RunID: fmt.Sprintf("prior-%s-%d", arm, i), Stage: "build", Arm: arm, Window: "train", Success: true, Reward: 1, RewardSet: true, Assigned: uint64(i + 1)})
		}
	}
	return observations
}

func seedExperimentJournals(t *testing.T, runsDir string, observations []bandit.Observation) {
	t.Helper()
	if len(observations) == 0 {
		return
	}
	run, err := journal.Create(runsDir, journal.RunIdentity{RunID: "experiment-history", Workflow: "fixture", WorkflowVersion: 1, WorkflowDigest: "sha256:fixture", Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := run.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, observation := range observations {
		if err := (bandit.Config{Stage: observation.Stage}).RecordObservation(observation, run); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExperimentRetryAndRepassMatchRunner(t *testing.T) {
	for _, mode := range []string{"retry", "repass"} {
		t.Run(mode, func(t *testing.T) {
			c := experimentParityCase(42)
			if mode == "retry" {
				c.Spec.Tasks[0].Retry = &apiv1.RetryPolicy{MaxAttempts: 2}
				c.Script["build"] = []scriptedCall{{err: errors.New("scripted attempt failure")}, succeed(nil)}
			} else {
				c.Spec.Gates[0].Branches["fail"] = "build"
				c.Script["build"] = []scriptedCall{fail("nonzero_exit", "retry me"), succeed(nil)}
			}
			local := runParityRunnerSide(t, c, "experiment-"+mode)
			plane := &experimentTestPlane{}
			cloud := runExperimentQualification(t, c, "experiment-"+mode, plane, false)
			if cloud.Err != nil || local.Err != nil {
				t.Fatalf("cloud=%v local=%v", cloud.Err, local.Err)
			}
			if got, want := experimentJournalDecisions(t, cloud.Events), experimentJournalDecisions(t, local.Events); !reflect.DeepEqual(got, want) {
				t.Fatalf("cloud decisions %v; local %v", got, want)
			}
			if plane.reads != 2 || len(plane.writes) != 2 {
				t.Fatalf("reads=%d writes=%v", plane.reads, plane.writes)
			}
			first, second := plane.writes[0].Key, plane.writes[1].Key
			if first == second {
				t.Fatalf("attempt identities alias: %+v", first)
			}
			if mode == "retry" && (first.Occurrence != second.Occurrence || second.Attempt != 2) {
				t.Fatalf("retry identity changed occurrence: %v %v", first, second)
			}
			if mode == "repass" && first.Occurrence == second.Occurrence {
				t.Fatalf("repass reused occurrence: %v %v", first, second)
			}
		})
	}
}

func TestExperimentVariantPlacementChecksFinalInputs(t *testing.T) {
	for _, mode := range []string{"variant-needs-instance", "variant-clears-instance", "upstream-overrides-variant"} {
		t.Run(mode, func(t *testing.T) {
			c := experimentParityCase(7)
			task := &c.Spec.Tasks[0]
			task.Inputs["kind"] = "shell"
			task.Experiment.Arms[0].Variant["kind"] = "some-future-kind"
			if mode == "variant-clears-instance" {
				task.Inputs["kind"] = "some-future-kind"
				task.Experiment.Arms[0].Variant["kind"] = "shell"
			}
			if mode == "upstream-overrides-variant" {
				task.InputsFrom = map[string]string{"kind": "kind"}
				seed := detTask("seed", "build")
				c.Spec.Tasks = append(c.Spec.Tasks, seed)
				c.Spec.Start = "seed"
				c.Script["seed"] = []scriptedCall{succeed(map[string]any{"kind": "shell"})}
			}
			plane := &experimentTestPlane{}
			// Only build is remote; the seed activity in this fixture remains local.
			cloud := runExperimentQualification(t, c, "experiment-placement-"+mode, plane, true)
			if cloud.Err != nil {
				t.Fatal(cloud.Err)
			}
			refused := mode == "variant-needs-instance"
			dispatched := false
			for _, env := range cloud.Envelopes {
				if env.Stage == "build" {
					dispatched = true
				}
			}
			if dispatched == refused {
				t.Fatalf("refused=%v invocations=%v", refused, cloud.Envelopes)
			}
			if len(plane.writes) != 1 || plane.writes[0].Observation.Success == refused {
				t.Fatalf("wrong observation: %v", plane.writes)
			}
		})
	}
}
