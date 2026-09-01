package runner

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestBarrenUpstream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream []StageProduction
		want     []string
	}{
		{name: "no upstream at all is a genuine empty tick"},
		{
			name:     "one producing upstream substantiates the claim",
			upstream: []StageProduction{{Stage: "lens-a"}, {Stage: "lens-b", Delivered: true}},
		},
		{
			name:     "upstream that all journaled nothing is barren",
			upstream: []StageProduction{{Stage: "lens-a"}, {Stage: "lens-b"}},
			want:     []string{"lens-a", "lens-b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := BarrenUpstream(tc.upstream); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BarrenUpstream = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeliveredEvidence(t *testing.T) {
	if DeliveredEvidence(apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}) {
		t.Fatal("an empty result envelope delivered evidence")
	}
	if !DeliveredEvidence(apiv1.ResultEnvelope{Outputs: map[string]any{"count": 3}}) {
		t.Fatal("an output-carrying result delivered no evidence")
	}
	if !DeliveredEvidence(apiv1.ResultEnvelope{Artifacts: []apiv1.ArtifactPointer{{Path: "a.json"}}}) {
		t.Fatal("an artifact-carrying result delivered no evidence")
	}
}

// declaredUpstreamMachine is an analyze -> verdict chain where verdict NAMES
// analyze with contextFrom: the workflow author's declaration that analyze's
// artifacts are verdict's input, which is what makes an empty upstream
// unambiguous when verdict claims no-work.
func declaredUpstreamMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
		Start:    "analyze",
		Tasks: []apiv1.Task{
			{
				Name: "analyze", Type: apiv1.TaskDeterministic, Goal: "analyze the corpus",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "verdict",
			},
			{
				Name: "verdict", Type: apiv1.TaskDeterministic, Goal: "rule on the analyses",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, ContextFrom: []string{"analyze"},
			},
		},
	}
	m, err := workflow.Compile(
		workflow.Definition{Name: "declared-upstream-fixture", Version: 1, Spec: spec},
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile declared-upstream fixture: %v", err)
	}
	return m
}

func startDeclaredUpstreamRun(t *testing.T, runID string, byTask map[string]stubTaskResult) (Result, string) {
	t.Helper()
	r, runsDir := newTestRunner(t, byTask, nil)
	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: declaredUpstreamMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return res, runsDir
}

// TestRunnerRefusesNoWorkWhenDeclaredUpstreamDeliveredNothing is #2736's core
// acceptance: a stage that consumes upstream evidence by declaration and finds
// none of it cannot complete the run on its word alone. The run is reportable
// as failed, with the barren upstream named.
func TestRunnerRefusesNoWorkWhenDeclaredUpstreamDeliveredNothing(t *testing.T) {
	const runID = "run-no-work-barren-upstream"
	res, runsDir := startDeclaredUpstreamRun(t, runID, map[string]stubTaskResult{
		runID + ":analyze": {status: apiv1.ResultSuccess},
		runID + ":verdict": {status: apiv1.ResultNoWork, summary: "nothing to rule on"},
	})
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed — a no-work claim whose evidence never arrived must not read as a healthy empty tick", res.Phase)
	}
	if res.FailureStage != "verdict" || res.FailureCode != NoWorkUnsubstantiatedCode {
		t.Fatalf("failure = %q/%q, want verdict/%s", res.FailureStage, res.FailureCode, NoWorkUnsubstantiatedCode)
	}
	if res.NoWork {
		t.Fatal("refused no-work claim was still exposed to the scheduler as an idle poll")
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var annotated bool
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == "no-work-unsubstantiated" {
			annotated = true
			barren, ok := event.Runner["barrenUpstreamStages"].([]any)
			if !ok || len(barren) != 1 || barren[0] != "analyze" {
				t.Fatalf("annotation named %v, want [analyze]", event.Runner["barrenUpstreamStages"])
			}
		}
	}
	if !annotated {
		t.Fatal("refusal recorded no runner annotation naming the missing evidence")
	}
}

// TestRunnerAcceptsNoWorkWhenDeclaredUpstreamDelivered is the other half: one
// producing upstream stage is enough for the claim to be the honest "looked
// and found nothing", which still short-circuits to completed (#233).
func TestRunnerAcceptsNoWorkWhenDeclaredUpstreamDelivered(t *testing.T) {
	const runID = "run-no-work-substantiated"
	res, _ := startDeclaredUpstreamRun(t, runID, map[string]stubTaskResult{
		runID + ":analyze": {status: apiv1.ResultSuccess, outputs: map[string]any{"candidates": 4}},
		runID + ":verdict": {status: apiv1.ResultNoWork, summary: "looked at 4 candidates, none actionable"},
	})
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if res.FailureCode != "" {
		t.Fatalf("failureCode = %q, want empty", res.FailureCode)
	}
}

// TestRunnerAcceptsNoWorkWithoutDeclaredUpstream pins the check's scope: a
// stage that never declared it consumes upstream artifacts is not judged on
// them, because its predecessor may have delivered through the shared
// workspace and returned an empty envelope.
func TestRunnerAcceptsNoWorkWithoutDeclaredUpstream(t *testing.T) {
	const runID = "run-no-work-undeclared-upstream"
	r, _ := newTestRunner(t, map[string]stubTaskResult{
		runID + ":query-backlog": {status: apiv1.ResultSuccess},
		runID + ":curate":        {status: apiv1.ResultNoWork, summary: "nothing to curate"},
	}, nil)
	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: noWorkFixtureMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
}

// TestRunnerRefusesFanInNoWorkWhenNoBranchDelivered is the issue's headline
// case: every branch analysis succeeded, none journaled anything, and the join
// has no basis for any verdict at all — so the run must not complete as if it
// had one.
func TestRunnerRefusesFanInNoWorkWhenNoBranchDelivered(t *testing.T) {
	const runID = "run-fan-in-no-work-barren"
	byTask := map[string]stubTaskResult{
		runID + ":lens-a":  {status: apiv1.ResultSuccess},
		runID + ":lens-b":  {status: apiv1.ResultSuccess},
		runID + ":lens-c":  {status: apiv1.ResultSuccess},
		runID + ":collate": {status: apiv1.ResultNoWork, summary: "no analyses to collate"},
	}
	r, _ := newParallelTestRunner(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: byTask}, nil
	})
	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo",
		Machine: parallelRunnerMachine(t, 1, apiv1.WorkspaceScratch),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed || res.FailureCode != NoWorkUnsubstantiatedCode {
		t.Fatalf("phase/code = %q/%q, want failed/%s", res.Phase, res.FailureCode, NoWorkUnsubstantiatedCode)
	}
}

// TestRunnerAcceptsFanInNoWorkWhenABranchDelivered keeps the fan-in check
// honest in the other direction: one branch's output is evidence the join
// actually looked at.
func TestRunnerAcceptsFanInNoWorkWhenABranchDelivered(t *testing.T) {
	const runID = "run-fan-in-no-work-substantiated"
	byTask := map[string]stubTaskResult{
		runID + ":lens-a":  {status: apiv1.ResultSuccess},
		runID + ":lens-b":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 2}},
		runID + ":lens-c":  {status: apiv1.ResultSuccess},
		runID + ":collate": {status: apiv1.ResultNoWork, summary: "findings were all already tracked"},
	}
	r, _ := newParallelTestRunner(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: byTask}, nil
	})
	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo",
		Machine: parallelRunnerMachine(t, 1, apiv1.WorkspaceScratch),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
}
