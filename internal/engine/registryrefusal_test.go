package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/engineadmission"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// r9Task returns a minimal deterministic task the registry accepts, so each
// refusal fixture differs from the next in exactly one declaration.
func r9Task(name string) apiv1.Task {
	return apiv1.Task{
		Name: name, Type: apiv1.TaskDeterministic, Goal: name,
		Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
	}
}

func r9Spec(tasks ...apiv1.Task) apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    tasks[0].Name,
		Tasks:    tasks,
	}
}

// TestAdmissionRefusesUnsupportedEngineFeatures is the R9 refusal table: each
// declaration the engine walk does not implement is refused at run start with
// its own named sentinel, naming the declaring stage. Loud, not silent — the
// walk would otherwise execute the definition as if the declaration were
// absent.
func TestAdmissionRefusesUnsupportedEngineFeatures(t *testing.T) {
	experiment := r9Task("implement")
	experiment.Experiment = &apiv1.BanditExperiment{}

	cases := []struct {
		name     string
		spec     apiv1.WorkflowSpec
		sentinel error
		wantIn   string
	}{
		{"bandit experiment", r9Spec(experiment), ErrExperimentUnsupported, "task.experiment"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := engineadmission.RefuseDefinition("flow", tc.spec)
			if err == nil {
				t.Fatalf("RefuseDefinition admitted a definition declaring %s — the engine walk ignores it silently", tc.wantIn)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Errorf("error does not wrap the named sentinel %v: %v", tc.sentinel, err)
			}
			var refusal *UnsupportedFeatureError
			if !errors.As(err, &refusal) {
				t.Fatalf("error carries no *UnsupportedFeatureError: %v", err)
			}
			if refusal.Feature != tc.wantIn {
				t.Errorf("refusal names feature %q, want %q", refusal.Feature, tc.wantIn)
			}
			if !strings.Contains(err.Error(), "flow") || !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("refusal message must name the workflow and the declaration: %v", err)
			}
			if refusal.Feature != "spec.parallels" && refusal.Stage != "implement" {
				t.Errorf("refusal names stage %q, want the declaring stage %q", refusal.Stage, "implement")
			}
		})
	}
}

// TestAdmissionRefusesShippedParallels pins the spec-level half of R9 against
// the REAL declaration rather than a synthetic one: quality-sprint.yaml is the
// lane finding 002's grep found, so a hand-rolled Parallel{} that the compiler
// would reject anyway proves nothing about the refusal that matters.
func TestAdmissionRefusesShippedParallels(t *testing.T) {
	lane := loadProductionLane(t, "goobers", "quality-sprint")
	if len(lane.Spec.Parallels) == 0 {
		t.Fatal("quality-sprint no longer declares parallels — retarget this guard at whichever lane does, do not delete it")
	}
	err := engineadmission.RefuseDefinition(lane.Name, lane.Spec)
	if err == nil {
		t.Fatal("RefuseDefinition admitted a lane declaring parallels — the engine walk would run only its root path")
	}
	if !errors.Is(err, ErrParallelsUnsupported) {
		t.Errorf("error does not wrap ErrParallelsUnsupported: %v", err)
	}
	var refusal *UnsupportedFeatureError
	if !errors.As(err, &refusal) || refusal.Feature != "spec.parallels" {
		t.Errorf("refusal does not name spec.parallels: %v", err)
	}
	if refusal != nil && refusal.Stage != "" {
		t.Errorf("a spec-level refusal must name no stage, got %q", refusal.Stage)
	}
}

// TestRefusalMessageReadsAsOneSentence pins the string an operator actually
// reads off `goobers engine-start --workflow quality-sprint` — the far-side
// evidence this refusal exists to produce.
//
// It is a real assertion because the message was doubled: the sentinels each
// began "engine: " and UnsupportedFeaturesError prefixes "engine: refuse to
// start workflow %q: ", so the refusal read
//
//	engine: refuse to start workflow "quality-sprint": declares spec.parallels: engine: spec.parallels ...
//
// Counting the prefix rather than matching the whole string keeps this test
// about the defect (a doubled prefix) instead of freezing the wording.
func TestRefusalMessageReadsAsOneSentence(t *testing.T) {
	lane := loadProductionLane(t, "goobers", "quality-sprint")
	err := engineadmission.RefuseDefinition(lane.Name, lane.Spec)
	if err == nil {
		t.Fatal("RefuseDefinition admitted the shipped parallels lane")
	}
	msg := err.Error()
	if got := strings.Count(msg, "engine:"); got != 1 {
		t.Errorf("refusal says %q %d times, want exactly once (the wrapper owns the prefix; the sentinels must not repeat it):\n%s",
			"engine:", got, msg)
	}
	if !strings.HasPrefix(msg, `engine: refuse to start workflow "quality-sprint": `) {
		t.Errorf("refusal does not open by naming the refused workflow:\n%s", msg)
	}
	// The DSL path is said once, by the sentinel — not once by the wrapper and
	// again by the sentinel it wraps.
	if got := strings.Count(msg, "spec.parallels"); got != 1 {
		t.Errorf("refusal names %q %d times, want exactly once:\n%s", "spec.parallels", got, msg)
	}
	t.Logf("operator-facing refusal: %s", msg)
	// Each sentinel must still read as a whole sentence standalone, which is
	// how an errors.Is caller that prints the sentinel sees it.
	for _, sentinel := range []error{ErrParallelsUnsupported, ErrExperimentUnsupported} {
		if strings.Contains(sentinel.Error(), "engine:") {
			t.Errorf("sentinel %q carries its own \"engine:\" prefix, which doubles when wrapped", sentinel)
		}
		if !strings.Contains(sentinel.Error(), "engine walk") {
			t.Errorf("sentinel %q does not say what it is unimplemented ON", sentinel)
		}
	}
}

// TestAdmissionRefusalReportsEveryDeclaration pins the multi-refusal shape: a
// definition declaring several R9 features is refused once, with all of them
// named and each reachable through errors.Is, so an operator sees the whole
// set instead of fixing them one deploy at a time. It starts from the shipped
// parallels lane so the spec-level and task-level halves are exercised
// together on a definition the compiler actually admits.
func TestAdmissionRefusalReportsEveryDeclaration(t *testing.T) {
	lane := loadProductionLane(t, "goobers", "quality-sprint")
	spec := *lane.Spec.DeepCopy()
	if len(spec.Tasks) == 0 {
		t.Fatal("quality-sprint declares no tasks")
	}
	spec.Tasks[0].Experiment = &apiv1.BanditExperiment{}
	spec.Tasks[0].Limits = &apiv1.Limits{MaxCostUSD: 2}
	spec.Tasks[0].Outbox = []string{"out.json"}

	err := engineadmission.RefuseDefinition("flow", spec)
	if err == nil {
		t.Fatal("RefuseDefinition admitted a definition declaring unsupported features")
	}
	for _, sentinel := range []error{ErrParallelsUnsupported, ErrExperimentUnsupported} {
		if !errors.Is(err, sentinel) {
			t.Errorf("refusal does not reach sentinel %v: %v", sentinel, err)
		}
	}
	var multi *UnsupportedFeaturesError
	if !errors.As(err, &multi) {
		t.Fatalf("error is not an *UnsupportedFeaturesError: %v", err)
	}
	if len(multi.Refusals) != 2 {
		t.Errorf("refusal names %d declarations, want 2: %v", len(multi.Refusals), err)
	}
}

func TestAdmissionAdmitsOutbox(t *testing.T) {
	task := r9Task("implement")
	task.Outbox = []string{"reports/result.json"}
	if err := engineadmission.RefuseDefinition("flow", r9Spec(task)); err != nil {
		t.Fatalf("outbox definition refused after engine support landed: %v", err)
	}
}

// TestAdmissionAdmitsSupportedLimits guards duration enforcement support.
func TestAdmissionAdmitsSupportedLimits(t *testing.T) {
	task := r9Task("implement")
	task.Limits = &apiv1.Limits{MaxDurationSeconds: 600}
	if err := engineadmission.RefuseDefinition("flow", r9Spec(task)); err != nil {
		t.Fatalf("RefuseDefinition refused a duration-only limit the engine does enforce: %v", err)
	}
}
