package main

import (
	"testing"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/instance"
)

// backlogPartitionFixture is the run-controls fixture with the MIRC-2 claim
// partition configured the way the cloud instance configures it: a gaggle-level
// self identity and a gaggle RequireLabels naming this instance's half of a
// backlog it shares with the laptop instance.
func backlogPartitionFixture() (*instance.Config, *instance.ConfigSet) {
	cfg, set, _ := runControlsFixture()
	cfg.SelfIdentity = "instance-wide-bot"
	set.Gaggles[0].Spec.SelfIdentity = "goobersbot"
	set.Gaggles[0].Spec.RequireLabels = []string{"goobers:cloud", "area:web"}
	return cfg, set
}

// TestEngineStartSpecPinsBacklogQueryDefaults is #3873's pin on the
// engine-start path.
//
// The gaggle's RequireLabels and the instance's self identity are what keep a
// run inside its own claim partition: the runner injects them into every
// `goobers backlog-query` stage that does not declare its own
// (internal/runner/run.go:4413-4414) and the engine's runTask now does the
// same from RunInput. Both are pinned at START, so a starter that leaves them
// empty does not "resolve them later" — it dispatches a run whose
// backlog-query stage has no partition at all and claims the sibling
// instance's goobers:local items.
func TestEngineStartSpecPinsBacklogQueryDefaults(t *testing.T) {
	cfg, set := backlogPartitionFixture()
	spec, err := engineStartSpec(engineStartRequestFor(t, cfg, set, "web", "implementation"))
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}
	if got, want := spec.BacklogQueryAssignedTo, "goobersbot"; got != want {
		t.Fatalf("StartSpec.BacklogQueryAssignedTo = %q, want %q (the gaggle's self identity wins over the instance's)", got, want)
	}
	if got, want := spec.BacklogQueryRequireLabels, "goobers:cloud,area:web"; got != want {
		t.Fatalf("StartSpec.BacklogQueryRequireLabels = %q, want %q — an empty partition claims the whole shared backlog", got, want)
	}
}

// TestEngineStartBacklogQueryDefaultsMatchTheDaemon is the cross-starter
// assertion the E1 port rests on: engine-start must pin the SAME values the
// daemon's scheduler entry configures on its runner (daemon.go:1156-1157), or
// the two drivers partition the shared backlog differently and the lane's
// behaviour changes with which one started it.
func TestEngineStartBacklogQueryDefaultsMatchTheDaemon(t *testing.T) {
	cfg, set := backlogPartitionFixture()
	spec, err := engineStartSpec(engineStartRequestFor(t, cfg, set, "web", "implementation"))
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}
	if got, want := spec.BacklogQueryAssignedTo, selfIdentitiesByGaggle(cfg, set)["web"]; got != want {
		t.Fatalf("StartSpec.BacklogQueryAssignedTo = %q, but the daemon configures the runner with %q", got, want)
	}
	if got, want := spec.BacklogQueryRequireLabels, requireLabelsByGaggle(set)["web"]; got != want {
		t.Fatalf("StartSpec.BacklogQueryRequireLabels = %q, but the daemon configures the runner with %q", got, want)
	}
}

// TestEngineStartBacklogQueryDefaultsReachRunInput carries the pin one seam
// further, through the registry that builds the RunInput the workflow actually
// receives: runTask reads RunInput, so a spec field the registry drops never
// reaches the stage.
func TestEngineStartBacklogQueryDefaultsReachRunInput(t *testing.T) {
	cfg, set := backlogPartitionFixture()
	spec, err := engineStartSpec(engineStartRequestFor(t, cfg, set, "web", "implementation"))
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}
	reg, _, err := bootstrap.RegisterGaggleWorkflows(set, "web")
	if err != nil {
		t.Fatalf("register gaggle workflows: %v", err)
	}
	in, err := reg.StartInput("implementation", spec)
	if err != nil {
		t.Fatalf("StartInput: %v", err)
	}
	if got, want := in.BacklogQueryAssignedTo, "goobersbot"; got != want {
		t.Fatalf("RunInput.BacklogQueryAssignedTo = %q, want %q", got, want)
	}
	if got, want := in.BacklogQueryRequireLabels, "goobers:cloud,area:web"; got != want {
		t.Fatalf("RunInput.BacklogQueryRequireLabels = %q, want %q", got, want)
	}
}

// TestEngineStartBacklogQueryDefaultsEmptyWithoutConfiguration keeps the
// zero-declaration arm honest: an instance that configures neither identity
// nor RequireLabels pins neither, which is a no-op in the defaulting and
// leaves every stage's inputs exactly as declared — byte for byte as before
// this field existed.
func TestEngineStartBacklogQueryDefaultsEmptyWithoutConfiguration(t *testing.T) {
	cfg, set, _ := runControlsFixture()
	spec, err := engineStartSpec(engineStartRequestFor(t, cfg, set, "web", "implementation"))
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}
	if spec.BacklogQueryAssignedTo != "" || spec.BacklogQueryRequireLabels != "" {
		t.Fatalf("StartSpec pinned assignedTo=%q requireLabels=%q for an instance declaring neither",
			spec.BacklogQueryAssignedTo, spec.BacklogQueryRequireLabels)
	}
}

// TestEngineStartBacklogQueryAssignedToFallsBackToTheInstance pins the
// resolution order EffectiveSelfIdentity defines and selfIdentitiesByGaggle
// applies: a gaggle without its own identity inherits the instance's.
func TestEngineStartBacklogQueryAssignedToFallsBackToTheInstance(t *testing.T) {
	cfg, set := backlogPartitionFixture()
	set.Gaggles[0].Spec.SelfIdentity = ""
	spec, err := engineStartSpec(engineStartRequestFor(t, cfg, set, "web", "implementation"))
	if err != nil {
		t.Fatalf("engineStartSpec: %v", err)
	}
	if got, want := spec.BacklogQueryAssignedTo, "instance-wide-bot"; got != want {
		t.Fatalf("StartSpec.BacklogQueryAssignedTo = %q, want the instance identity %q", got, want)
	}
}
