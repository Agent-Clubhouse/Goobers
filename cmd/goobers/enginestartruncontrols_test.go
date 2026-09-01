package main

import (
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/workflow"
)

// runControlsFixture is one instance whose run-control policy is declared at
// EVERY layer of the #1671 hierarchy:
//
//	instance runConditions : maxRepasses 4, stalledRunTimeout 90m
//	repo override          : maxRunDuration 6h
//	gaggle spec            : maxRepasses 7
//	workflow spec          : stalledRunTimeout 2h
//
// apiv1.RunControls carries three fields and the hierarchy has four layers, so
// no single resolution can show all four surviving at once — one layer is
// always masked. The masking is chosen, not accidental: the gaggle masks the
// instance's maxRepasses and the workflow masks its stalledRunTimeout, while
// maxRunDuration is declared ONLY by the repo, so the fully-layered resolution
// carries a repo-only value that no other layer could have produced. (An
// earlier revision of this fixture had the workflow declare maxRunDuration 8h
// too, which masked the repo entirely: deleting the repo layer from
// resolveWorkflowRunControls left every test in this file green.)
//
// The instance layer is the one masked here, so it is covered by the
// "instance and repo only" arm of TestEngineStartSpecPinsResolvedRunControls,
// which strips the gaggle and workflow overrides and leaves 4 / 90m visible.
// Between the two arms, deleting any one of the four layers changes an
// asserted value.
//
// Fully layered, the resolved policy is maxRepasses 7 / stalledRunTimeout
// 2h0m0s / maxRunDuration 6h0m0s — none of which is a built-in default
// (3 / 45m / unset).
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
		Workflows: []apiv1.Workflow{runControlsWorkflow("implementation", "implement", &apiv1.RunControls{StalledRunTimeout: "2h"})},
	}
	return cfg, set, project
}

// runControlsWorkflow is one deterministic single-task workflow in the fixture
// gaggle. Deterministic so the same fixture compiles through the daemon's
// machine compiler as well as the engine registry, and single-task so the only
// thing distinguishing two of them is what they declare.
func runControlsWorkflow(name, task string, controls *apiv1.RunControls) apiv1.Workflow {
	return apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
			Start:    task,
			Tasks: []apiv1.Task{{
				Name: task,
				Type: apiv1.TaskDeterministic,
				Goal: "run " + task,
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			}},
			RunControls: controls,
		},
	}
}

// engineRunRequestFor builds the request `goobers engine-start` builds for
// this workflow, through the real registration path: RegisterGaggleWorkflows
// then reg.Latest, exactly as runEngineStart does. Going through the registry
// rather than hand-picking set.Workflows[i] matters — the definition the
// registry hands back is what the run's graph, placements and run controls
// must all be sourced from.
func engineRunRequestFor(t *testing.T, cfg *instance.Config, set *instance.ConfigSet, gaggle, workflowName string) engineRunRequest {
	t.Helper()
	reg, project, err := bootstrap.RegisterGaggleWorkflows(set, gaggle)
	if err != nil {
		t.Fatalf("register gaggle workflows: %v", err)
	}
	def, ok := reg.Latest(workflowName)
	if !ok {
		t.Fatalf("workflow %q is not registered", workflowName)
	}
	return engineRunRequest{
		cfg:       cfg,
		set:       set,
		gaggle:    gaggle,
		dedupeKey: "dedupe-1",
		project:   project,
		def:       def,
	}
}

// TestEngineStartSpecPinsResolvedRunControls is the seam the bug crossed
// (#3820): `goobers engine-start` built its StartSpec without ever resolving
// run controls, so the run it dispatched pinned the built-in 45m/3 defaults
// and the daemon's stalled-run watchdog enforced those against a workflow that
// had declared otherwise. Assert on the StartSpec the command actually builds,
// not on the resolver it calls — the resolver was never the broken part.
//
// The two arms together cover all four layers: see runControlsFixture.
func TestEngineStartSpecPinsResolvedRunControls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		declare func(*instance.ConfigSet)
		want    apiv1.RunControls
		because map[string]string
	}{
		{
			name:    "every layer declared",
			declare: func(*instance.ConfigSet) {},
			want: apiv1.RunControls{
				MaxRepasses:       7,
				StalledRunTimeout: "2h0m0s",
				MaxRunDuration:    "6h0m0s",
			},
			because: map[string]string{
				"maxRepasses":       "gaggle override over the instance's 4",
				"stalledRunTimeout": "workflow override over the instance's 90m",
				"maxRunDuration":    "repo override; no other layer declares this field",
			},
		},
		{
			// Strips the two innermost layers so the instance's own
			// runConditions become the surviving answer. Without this arm the
			// instance layer is masked at every field and could be deleted
			// unobserved.
			name: "instance and repo only",
			declare: func(set *instance.ConfigSet) {
				set.Gaggles[0].Spec.RunControls = nil
				set.Workflows[0].Spec.RunControls = nil
			},
			want: apiv1.RunControls{
				MaxRepasses:       4,
				StalledRunTimeout: "1h30m0s",
				MaxRunDuration:    "6h0m0s",
			},
			because: map[string]string{
				"maxRepasses":       "instance runConditions; 3 is the built-in default",
				"stalledRunTimeout": "instance runConditions 90m; 45m0s is the built-in default",
				"maxRunDuration":    "repo override; empty means the repo layer was dropped",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, set, _ := runControlsFixture()
			tc.declare(set)

			spec, err := engineRunSpec(engineRunRequestFor(t, cfg, set, "web", "implementation"))
			if err != nil {
				t.Fatalf("engineRunSpec: %v", err)
			}

			if got := spec.RunControls.MaxRepasses; got != tc.want.MaxRepasses {
				t.Errorf("pinned maxRepasses = %d, want %d (%s)", got, tc.want.MaxRepasses, tc.because["maxRepasses"])
			}
			if got := spec.RunControls.StalledRunTimeout; got != tc.want.StalledRunTimeout {
				t.Errorf("pinned stalledRunTimeout = %q, want %q (%s); %q is the built-in default, i.e. no configured layer reached the run",
					got, tc.want.StalledRunTimeout, tc.because["stalledRunTimeout"], runcontrol.DefaultStalledRunTimeout.String())
			}
			if got := spec.RunControls.MaxRunDuration; got != tc.want.MaxRunDuration {
				t.Errorf("pinned maxRunDuration = %q, want %q (%s)", got, tc.want.MaxRunDuration, tc.because["maxRunDuration"])
			}
		})
	}
}

// TestSchedulerDefinitionsPinResolvedRunControls is the daemon half of the
// seam engine-start now shares. buildSchedulerDefinitions is the daemon's only
// caller of resolveWorkflowRunControls, and until this test its single test
// caller passed an empty *instance.Config and asserted nothing about run
// controls — so reproducing #3820 on the daemon side (discarding every
// configured layer at that call site) left the whole package green, and the
// parity oracle below could not see it either, because it re-derives the
// daemon's answer by hand instead of calling the daemon.
//
// This asserts what the daemon's Starter actually carries: the resolved block
// pinned into every scheduled run of this workflow.
func TestSchedulerDefinitionsPinResolvedRunControls(t *testing.T) {
	cfg, set, _ := runControlsFixture()

	layout := instance.NewLayout(t.TempDir())
	if err := layout.EnsureGaggleRuntime("web"); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	var wg sync.WaitGroup
	definitions, err := buildSchedulerDefinitions(
		layout,
		cfg,
		set,
		nil,
		&wg,
		newDaemonRunnerRegistry(),
		nil,
		nil,
		nil,
		log,
		journal.NewRegistryScrubber(),
		nil,
		localscheduler.NewProviderQuotaState(),
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("build scheduler definitions: %v", err)
	}

	var entry *localscheduler.WorkflowEntry
	for i := range definitions.Entries {
		if definitions.Entries[i].Workflow == "implementation" && definitions.Entries[i].Gaggle == "web" {
			entry = &definitions.Entries[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("no scheduler entry for web/implementation in %d entries", len(definitions.Entries))
	}
	starter, ok := unwrapStarter(entry.Starter).(*trackedStarter)
	if !ok {
		t.Fatalf("scheduler entry Starter = %T, want *trackedStarter", entry.Starter)
	}

	want := apiv1.RunControls{MaxRepasses: 7, StalledRunTimeout: "2h0m0s", MaxRunDuration: "6h0m0s"}
	if starter.runControls != want {
		t.Errorf("daemon Starter pins %+v, want %+v; the scheduler entry dropped a configured layer "+
			"(maxRepasses 3 / stalledRunTimeout 45m0s would mean it resolved nothing at all)",
			starter.runControls, want)
	}
}

// TestEngineStartRunControlsMatchDaemonResolution: the two starters must agree.
// The daemon's scheduler entry hands controls.Overrides() to its Starter;
// engine-start must land on the identical block for the same config, or the
// same workflow gets a different run identity depending on who dispatched it.
//
// The daemon side here is an independent hand-derivation of the layer
// sequence rather than a call to the shared helper, so this stays an oracle
// rather than a tautology if either call site drifts. That the daemon really
// pins this block — not merely that the sequence produces it — is what
// TestSchedulerDefinitionsPinResolvedRunControls asserts against the real
// buildSchedulerDefinitions. The two together are the parity claim; this one
// alone is an oracle for the resolver only.
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

	spec, err := engineRunSpec(engineRunRequestFor(t, cfg, set, "web", "implementation"))
	if err != nil {
		t.Fatalf("engineRunSpec: %v", err)
	}

	if spec.RunControls != want {
		t.Errorf("engine-start pinned %+v, daemon pins %+v; the two starters disagree on the same workflow", spec.RunControls, want)
	}
}

// TestEngineStartRunControlsFollowRegisteredVersion: two workflow files can
// declare the same name in one gaggle. RegisterGaggleWorkflows registers both,
// so reg.Latest — the definition engine-start dispatches — is the LAST one,
// while a forward scan of set.Workflows finds the FIRST. Resolving the run
// controls by name rather than from the registered definition would pin v2's
// graph against v1's watchdog budget: one run, two versions.
func TestEngineStartRunControlsFollowRegisteredVersion(t *testing.T) {
	cfg, set, _ := runControlsFixture()
	set.Workflows = append(set.Workflows, runControlsWorkflow(
		"implementation", "implement-v2", &apiv1.RunControls{StalledRunTimeout: "99h"},
	))

	req := engineRunRequestFor(t, cfg, set, "web", "implementation")
	if req.def.Version != 2 || req.def.Spec.Start != "implement-v2" {
		t.Fatalf("registry returned version %d start %q, want the second declaration (v2, implement-v2)",
			req.def.Version, req.def.Spec.Start)
	}

	spec, err := engineRunSpec(req)
	if err != nil {
		t.Fatalf("engineRunSpec: %v", err)
	}
	if got := spec.RunControls.StalledRunTimeout; got != "99h0m0s" {
		t.Errorf("pinned stalledRunTimeout = %q, want 99h0m0s from the registered definition; "+
			"%q is the first same-named declaration's, i.e. the run would enforce v1's budget on v2's graph",
			got, "2h0m0s")
	}
}

// TestEngineStartRunControlsRejectsUndeclaredWorkflow: resolution is
// layer-complete or it fails. Silently falling back to the defaults for a
// workflow the config set does not carry is exactly the failure mode #3820 is
// about, so an unresolvable workflow must error rather than pin 45m/3.
func TestEngineStartRunControlsRejectsUndeclaredWorkflow(t *testing.T) {
	cfg, set, project := runControlsFixture()
	req := engineRunRequest{
		cfg:     cfg,
		set:     set,
		gaggle:  "web",
		project: project,
		def:     workflow.Definition{Name: "not-declared"},
	}
	if _, err := engineRunControls(req); err == nil {
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
	req := engineRunRequest{
		cfg:     cfg,
		set:     set,
		gaggle:  "web",
		project: project,
		def:     workflow.Definition{Name: "implementation", Spec: set.Workflows[0].Spec},
	}
	if _, err := engineRunControls(req); err == nil {
		t.Fatal("invalid workflow stalledRunTimeout was accepted; want a dispatch-time error")
	}
}
