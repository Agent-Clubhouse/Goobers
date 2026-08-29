package engine

import (
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	wf "github.com/goobers/goobers/internal/workflow"
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

// TestStartInputRefusesUnsupportedEngineFeatures is the R9 refusal table: each
// declaration the engine walk does not implement is refused at run start with
// its own named sentinel, naming the declaring stage. Loud, not silent — the
// walk would otherwise execute the definition as if the declaration were
// absent.
func TestStartInputRefusesUnsupportedEngineFeatures(t *testing.T) {
	experiment := r9Task("implement")
	experiment.Experiment = &apiv1.BanditExperiment{}

	tokens := r9Task("implement")
	tokens.Limits = &apiv1.Limits{MaxTokens: 100000}

	cost := r9Task("implement")
	cost.Limits = &apiv1.Limits{MaxCostUSD: 5}

	outbox := r9Task("implement")
	outbox.Outbox = []string{"dist/report.json"}

	parallels := r9Spec(r9Task("implement"))
	parallels.Parallels = []apiv1.Parallel{{Name: "fan"}}

	cases := []struct {
		name     string
		spec     apiv1.WorkflowSpec
		sentinel error
		wantIn   string
	}{
		{"bandit experiment", r9Spec(experiment), ErrExperimentUnsupported, "task.experiment"},
		{"cumulative token budget", r9Spec(tokens), ErrUsageLimitsUnsupported, "task.limits.maxTokens/maxCostUSD"},
		{"cumulative cost budget", r9Spec(cost), ErrUsageLimitsUnsupported, "task.limits.maxTokens/maxCostUSD"},
		{"outbox export", r9Spec(outbox), ErrOutboxUnsupported, "task.outbox"},
		{"parallel fan-out", parallels, ErrParallelsUnsupported, "spec.parallels"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistryWithPreviewFeatures(true)
			if _, err := r.RegisterDefinition(wf.Definition{Name: "flow", Spec: tc.spec}); err != nil {
				t.Fatalf("RegisterDefinition: %v", err)
			}
			_, err := r.StartInput("flow", StartSpec{RunID: "run-1", Gaggle: "web"})
			if err == nil {
				t.Fatalf("StartInput admitted a definition declaring %s — the engine walk ignores it silently", tc.wantIn)
			}
			if !errIsUnsupportedFeature(err, tc.sentinel) {
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

// TestStartInputRefusalReportsEveryDeclaration pins the multi-refusal shape: a
// definition declaring several R9 features is refused once, with all of them
// named and each reachable through errors.Is, so an operator sees the whole
// set instead of fixing them one deploy at a time.
func TestStartInputRefusalReportsEveryDeclaration(t *testing.T) {
	task := r9Task("implement")
	task.Experiment = &apiv1.BanditExperiment{}
	task.Limits = &apiv1.Limits{MaxCostUSD: 2}
	task.Outbox = []string{"out.json"}
	spec := r9Spec(task)
	spec.Parallels = []apiv1.Parallel{{Name: "fan"}}

	r := NewRegistryWithPreviewFeatures(true)
	if _, err := r.RegisterDefinition(wf.Definition{Name: "flow", Spec: spec}); err != nil {
		t.Fatalf("RegisterDefinition: %v", err)
	}
	_, err := r.StartInput("flow", StartSpec{RunID: "run-1", Gaggle: "web"})
	if err == nil {
		t.Fatal("StartInput admitted a definition declaring four unsupported features")
	}
	for _, sentinel := range []error{
		ErrParallelsUnsupported, ErrExperimentUnsupported, ErrUsageLimitsUnsupported, ErrOutboxUnsupported,
	} {
		if !errors.Is(err, sentinel) {
			t.Errorf("refusal does not reach sentinel %v: %v", sentinel, err)
		}
	}
	var multi *UnsupportedFeaturesError
	if !errors.As(err, &multi) {
		t.Fatalf("error is not an *UnsupportedFeaturesError: %v", err)
	}
	if len(multi.Refusals) != 4 {
		t.Errorf("refusal names %d declarations, want 4: %v", len(multi.Refusals), err)
	}
}

// TestStartInputAdmitsSupportedLimits guards the boundary: only the cumulative
// usage budgets are refused. maxDurationSeconds IS enforced on the engine
// (the stage activity's StartToCloseTimeout), so a definition declaring only
// that must still start — otherwise the refusal is a regression, not a guard.
func TestStartInputAdmitsSupportedLimits(t *testing.T) {
	task := r9Task("implement")
	task.Limits = &apiv1.Limits{MaxDurationSeconds: 600}
	r := NewRegistryWithPreviewFeatures(true)
	if _, err := r.RegisterDefinition(wf.Definition{Name: "flow", Spec: r9Spec(task)}); err != nil {
		t.Fatalf("RegisterDefinition: %v", err)
	}
	if _, err := r.StartInput("flow", StartSpec{RunID: "run-1", Gaggle: "web"}); err != nil {
		t.Fatalf("StartInput refused a duration-only limit the engine does enforce: %v", err)
	}
}

// TestRegisterDefinitionStillAdmitsUnsupportedFeatures is the blast-radius
// guard, and the reason the refusal lives at StartInput rather than at
// registration.
//
// internal/bootstrap.RegisterGaggleWorkflows registers EVERY workflow of a
// gaggle into one registry and returns on the first error. The goobers gaggle
// ships quality-sprint.yaml, which declares parallels. Refusing at
// registration would therefore fail the whole registry build and take
// backlog-curation — the first lane scheduled to move to the engine — offline
// along with it. This test pins that a gaggle containing an unsupported lane
// still builds a registry whose OTHER lanes start.
func TestRegisterDefinitionStillAdmitsUnsupportedFeatures(t *testing.T) {
	unsupported := r9Spec(r9Task("implement"))
	unsupported.Parallels = []apiv1.Parallel{{Name: "fan"}}

	r := NewRegistryWithPreviewFeatures(true)
	if _, err := r.RegisterDefinition(wf.Definition{Name: "quality-sprint", Spec: unsupported}); err != nil {
		t.Fatalf("RegisterDefinition must not refuse — refusing here fails the whole gaggle registry build: %v", err)
	}
	if _, err := r.RegisterDefinition(wf.Definition{Name: "backlog-curation", Spec: r9Spec(r9Task("query-backlog"))}); err != nil {
		t.Fatalf("RegisterDefinition: %v", err)
	}
	if _, err := r.StartInput("backlog-curation", StartSpec{RunID: "run-1", Gaggle: "web"}); err != nil {
		t.Fatalf("an unsupported lane in the same registry must not block a supported one: %v", err)
	}
	if _, err := r.StartInput("quality-sprint", StartSpec{RunID: "run-2", Gaggle: "web"}); !errors.Is(err, ErrParallelsUnsupported) {
		t.Fatalf("StartInput on the unsupported lane must refuse with the named sentinel, got %v", err)
	}
}

// TestShippedGaggleRegistersAndRefusesOnlyDeclaringLanes runs the guard above
// against the REAL shipped config tree rather than a synthetic pair: the
// goobers gaggle's registry must build, backlog-curation and implementation
// must start, and only the lanes that actually declare an R9 feature are
// refused. This is what would have caught the outage shape at review time.
func TestShippedGaggleRegistersAndRefusesOnlyDeclaringLanes(t *testing.T) {
	set, report, err := instance.LoadConfigDir(referenceWorkflowsRoot())
	if err != nil {
		t.Fatalf("load reference-workflows: %v\n%v", err, report)
	}
	r := NewRegistryWithPreviewFeatures(true)
	var names []string
	for i := range set.Workflows {
		w := set.Workflows[i]
		if w.Spec.Gaggle != "goobers" {
			continue
		}
		if _, err := r.RegisterDefinition(wf.Definition{Name: w.Name, DSLVersion: w.DSLVersion, Spec: w.Spec}); err != nil {
			t.Fatalf("registering shipped lane %q failed — RegisterGaggleWorkflows would abort the whole gaggle: %v", w.Name, err)
		}
		names = append(names, w.Name)
	}
	if len(names) < 2 {
		t.Fatalf("expected the goobers gaggle to ship several lanes, got %v", names)
	}

	refused := map[string]bool{}
	for _, name := range names {
		if _, err := r.StartInput(name, StartSpec{RunID: "run-" + name, Gaggle: "goobers"}); err != nil {
			var multi *UnsupportedFeaturesError
			if !errors.As(err, &multi) {
				t.Fatalf("shipped lane %q refused for a non-R9 reason: %v", name, err)
			}
			refused[name] = true
		}
	}
	for _, mustStart := range []string{"backlog-curation", "implementation"} {
		if refused[mustStart] {
			t.Errorf("lane %q is on the engine cutover path and must start, but the R9 refusal rejected it", mustStart)
		}
	}
	if !refused["quality-sprint"] {
		t.Error("quality-sprint declares parallels and must be refused; if the lane changed, retarget this guard rather than deleting it")
	}
}
