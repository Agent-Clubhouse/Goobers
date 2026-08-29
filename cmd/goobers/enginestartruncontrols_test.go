package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/workflow"
)

// runControlsFixture is one instance whose run-control policy is configured at
// EVERY layer of the #1671 hierarchy, each contributing a distinguishable
// field, so a starter that drops a layer produces a value no other layer could
// have produced:
//
//	instance runConditions : stalledRunTimeout 90m, maxRepasses 4
//	repo override          : maxRunDuration 6h
//	gaggle spec            : maxRepasses 7
//	workflow spec          : maxRunDuration 8h
//
// The resolved policy is therefore 90m / 7 / 8h — none of which is a default.
func runControlsFixture() (*instance.Config, *instance.ConfigSet, apiv1.RepoRef) {
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	cfg := &instance.Config{
		Repos: []instance.RepoRef{{
			Provider:    "github",
			Owner:       "acme",
			Name:        "web",
			RunControls: &apiv1.RunControls{MaxRunDuration: "6h"},
		}},
		RunConditions: instance.RunConditions{
			MaxRepasses:       4,
			StalledRunTimeout: "90m",
		},
	}
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{
			ObjectMeta: metav1.ObjectMeta{Name: "web"},
			Spec: apiv1.GaggleSpec{
				Project:     project,
				RunControls: &apiv1.RunControls{MaxRepasses: 7},
			},
		}},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
			Spec: apiv1.WorkflowSpec{
				Gaggle:      "web",
				Triggers:    []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
				Start:       "implement",
				Tasks:       []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement"}},
				RunControls: &apiv1.RunControls{MaxRunDuration: "8h"},
			},
		}},
	}
	return cfg, set, project
}

// TestEngineStartSpecPinsResolvedRunControls is the seam the bug crossed
// (#3820): `goobers engine-start` built its StartSpec without ever resolving
// run controls, so the run it dispatched pinned the built-in 45m/3 defaults
// and the daemon's stalled-run watchdog enforced those against a workflow that
// had declared otherwise. Assert on the StartSpec the command actually builds,
// not on the resolver it calls — the resolver was never the broken part.
func TestEngineStartSpecPinsResolvedRunControls(t *testing.T) {
	cfg, set, project := runControlsFixture()
	def := workflow.Definition{Name: "implementation", Spec: set.Workflows[0].Spec}

	spec, err := engineStartSpec(cfg, set, "web", "implementation", "dedupe-1", project, def, false)
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}

	if got := spec.RunControls.StalledRunTimeout; got != "1h30m0s" {
		t.Errorf("pinned stalledRunTimeout = %q, want 1h30m0s (instance runConditions 90m); "+
			"%q is the built-in default, i.e. no configured layer reached the run",
			got, runcontrol.DefaultStalledRunTimeout.String())
	}
	if got := spec.RunControls.MaxRepasses; got != 7 {
		t.Errorf("pinned maxRepasses = %d, want 7 (gaggle override)", got)
	}
	if got := spec.RunControls.MaxRunDuration; got != "8h0m0s" {
		t.Errorf("pinned maxRunDuration = %q, want 8h0m0s (workflow override over the repo's 6h)", got)
	}
}

// TestEngineStartRunControlsMatchDaemonResolution: the two starters must agree.
// The daemon's scheduler entry (schedulerDefinitions) resolves instance ->
// repo -> gaggle -> workflow and hands controls.Overrides() to its Starter;
// engine-start must land on the identical block for the same config, or the
// same workflow gets a different run identity depending on who dispatched it.
// The expectation is spelled out here rather than delegated to the shared
// helper, so this stays an independent oracle if either call site drifts.
func TestEngineStartRunControlsMatchDaemonResolution(t *testing.T) {
	cfg, set, project := runControlsFixture()

	instanceControls := cfg.RunConditions.RunControls()
	repo, ok := configuredRepoForProject(cfg, project)
	if !ok {
		t.Fatal("fixture repo did not match the gaggle project; the repo layer would be untested")
	}
	instanceControls = repo.EffectiveRunControls(instanceControls)
	daemonControls, err := runcontrol.Resolve(
		instanceControls,
		set.Gaggles[0].Spec.RunControls,
		set.Workflows[0].Spec.RunControls,
	)
	if err != nil {
		t.Fatalf("daemon-side resolve: %v", err)
	}
	want := daemonControls.Overrides()

	def := workflow.Definition{Name: "implementation", Spec: set.Workflows[0].Spec}
	spec, err := engineStartSpec(cfg, set, "web", "implementation", "dedupe-1", project, def, false)
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}

	if spec.RunControls != want {
		t.Errorf("engine-start pinned %+v, daemon pins %+v; the two starters disagree on the same workflow", spec.RunControls, want)
	}
}

// TestEngineStartRunControlsRejectsUndeclaredWorkflow: resolution is
// layer-complete or it fails. Silently falling back to the defaults for a
// workflow the config set does not carry is exactly the failure mode #3820 is
// about, so an unresolvable workflow must error rather than pin 45m/3.
func TestEngineStartRunControlsRejectsUndeclaredWorkflow(t *testing.T) {
	cfg, set, project := runControlsFixture()
	if _, err := engineStartRunControls(cfg, set, "web", "not-declared", project); err == nil {
		t.Fatal("resolving controls for an undeclared workflow succeeded; want an error")
	}
}

// TestEngineStartRunControlsPropagateInvalidDuration: an unparseable override
// must surface at dispatch. runcontrol.Resolve's apply path deliberately
// propagates parse failures instead of resolving them to zero (an unlimited
// run); engine-start must not swallow that on the way out.
func TestEngineStartRunControlsPropagateInvalidDuration(t *testing.T) {
	cfg, set, project := runControlsFixture()
	set.Workflows[0].Spec.RunControls = &apiv1.RunControls{StalledRunTimeout: "ninety minutes"}
	if _, err := engineStartRunControls(cfg, set, "web", "implementation", project); err == nil {
		t.Fatal("invalid workflow stalledRunTimeout was accepted; want a dispatch-time error")
	}
}
