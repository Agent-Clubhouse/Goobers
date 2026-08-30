package main

// Tests for #3987: checkpoint 3's local-executable refusal must not fire on a
// lane the per-entry engine selection (#3876) routes to the tier-3 engine.
//
// The defect these pin was a production outage: sixteen workflows on the live
// instance — including the backlog-curation and defect-nomination lanes —
// were boot-refused the moment they declared runsOn, because
// placementRefusals solved every workflow against the daemon's SELF-ONLY
// substrate and daemon.go stamped the result onto the scheduler entry
// unconditionally, including for entries whose stages
// bootstrap.PinStagePlacements had already placed on the FULL declared
// inventory. Adding runsOn to a lane took it off the air.
//
// The carve-out is deliberately narrow, and every boundary is asserted here:
// only an entry selectEngineForEntry actually routes to the engine is exempt.
// A runner-driven entry (self-pinned or mixed), an engine-disabled instance,
// and a lane no runner in the FULL inventory can satisfy all keep today's
// fail-closed refusal.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

// seamGaggle is the gaggle every in-memory seam fixture below declares.
const seamGaggle = "example"

// seamIdentity is the identity every in-memory seam fixture's lane carries.
var seamIdentity = localscheduler.WorkflowIdentity{Gaggle: seamGaggle, Workflow: "pod-pinned"}

// seamConfig builds the #3987 inventory in memory: a linux self runner plus a
// non-self IMAGE runner claiming windows — the shape a real pod-pinned
// instance declares. engineEnabled toggles the only thing that distinguishes
// the two halves of the carve-out.
//
// Built by hand rather than loaded from disk on purpose: RNR002 refuses a
// remote runner on an instance with engine projection off (runners.go:386),
// so the engine-DISABLED half of the ablation is not expressible as a
// loadable instance.yaml at all. The seam is the only place it can be
// asserted, and asserting it is what proves the fix did not silently widen
// the exemption to every declared inventory.
func seamConfig(engineEnabled bool) *instance.Config {
	cfg := &instance.Config{
		Runners: []instance.RunnerEntry{
			{Name: "self", Host: instance.RunnerHostSelfName, Provides: instance.RunnerProvides{OS: "linux"}},
			{Name: "ci", Host: "ghcr.io/example/ci:v1", Provides: instance.RunnerProvides{OS: "windows"}},
		},
	}
	if engineEnabled {
		cfg.Engine = &instance.EngineConfig{
			HostPort:  "temporal.example:7233",
			Namespace: "default",
			TaskQueue: "goobers",
		}
	}
	return cfg
}

func seamSet() *instance.ConfigSet {
	return &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: seamGaggle}}},
	}
}

// seamTask is one 3.0 deterministic stage. A "goobers" command is a BUILTIN,
// which derives no run:shell tag and therefore does not pin itself to self;
// any other command is a shell stage, which does.
func seamTask(name string, builtin bool, runsOn *apiv1.RunsOn, next string) apiv1.Task {
	command := []string{"true"}
	if builtin {
		command = []string{"goobers", "docs-churn"}
	}
	return apiv1.Task{
		Name:   name,
		Type:   apiv1.TaskDeterministic,
		Goal:   "run " + name,
		RunsOn: runsOn,
		Run:    &apiv1.DeterministicRun{Command: command, Workspace: apiv1.WorkspaceScratch},
		Next:   next,
	}
}

func seamDefinition(tasks ...apiv1.Task) workflow.Definition {
	return workflow.Definition{
		Name:       seamIdentity.Workflow,
		Version:    1,
		DSLVersion: "3.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle: seamGaggle,
			Start:  tasks[0].Name,
			Tasks:  tasks,
		},
	}
}

func seamMachines(def workflow.Definition) map[localscheduler.WorkflowIdentity]*workflow.Machine {
	return map[localscheduler.WorkflowIdentity]*workflow.Machine{seamIdentity: {Def: def}}
}

// podPinnedDefinition is the #3987 shape: EVERY stage pod-pinned onto the
// declared non-self runner, no self-only stage anywhere — what
// backlog-curation became when it was migrated to DSL 3.0 with runsOn.
func podPinnedDefinition() workflow.Definition {
	return seamDefinition(seamTask("build", true, &apiv1.RunsOn{OS: "windows"}, ""))
}

// seamDecisions runs the daemon's own two-pass sequence — engine selection,
// then checkpoint 3 — exactly as buildSchedulerSetup does, so a test cannot
// pass by asserting an ordering the daemon does not use.
func seamDecisions(t *testing.T, cfg *instance.Config, def workflow.Definition) placementDecisions {
	t.Helper()
	set := seamSet()
	machines := seamMachines(def)
	selections, err := engineSelections(cfg, set, machines)
	if err != nil {
		t.Fatalf("engineSelections: %v", err)
	}
	decisions, err := placementRefusals(cfg, set, nil, machines, selections)
	if err != nil {
		t.Fatalf("placementRefusals: %v", err)
	}
	return decisions
}

// TestEngineSelectedLaneIsExemptFromLocalExecutableRefusal is the #3987
// defect, asserted at the seam that produced it: a fully pod-pinned lane on an
// engine-enabled instance is NOT refused, even though the daemon's own
// self-only substrate cannot place a single one of its stages.
//
// The two-sided assertion matters. Dropping the refusal without keeping the
// diagnostic would silence the one signal an operator has when the engine
// later declines the lane, so the solve's diagnostic must survive as a
// deferral rather than being discarded.
func TestEngineSelectedLaneIsExemptFromLocalExecutableRefusal(t *testing.T) {
	decisions := seamDecisions(t, seamConfig(true), podPinnedDefinition())

	if diagnostic, refused := decisions.Refusals[seamIdentity]; refused {
		t.Fatalf("a fully pod-pinned engine-selected lane must not be boot-refused (#3987): %s", diagnostic)
	}
	deferred, ok := decisions.EngineDeferred[seamIdentity]
	if !ok {
		t.Fatal("the exempted lane's substrate diagnostic must be preserved as a deferral, not discarded")
	}
	for _, want := range []string{`stage "build"`, "ci (host: ghcr.io/example/ci:v1)"} {
		if !strings.Contains(deferred, want) {
			t.Errorf("deferred diagnostic missing %q: %s", want, deferred)
		}
	}
}

// TestEngineDisabledPodPinnedLaneStillRefuses is the ABLATION: the identical
// lane and the identical inventory, with the engine switched off, keeps
// today's fail-closed refusal. Without this, "exempt the pod-pinned lane"
// would be indistinguishable from "stop refusing pod-pinned lanes" — and the
// latter would green-light a lane on an instance that has nowhere to dispatch
// it, which is the failure #2860's refusal exists to prevent.
func TestEngineDisabledPodPinnedLaneStillRefuses(t *testing.T) {
	decisions := seamDecisions(t, seamConfig(false), podPinnedDefinition())

	diagnostic, refused := decisions.Refusals[seamIdentity]
	if !refused {
		t.Fatal("with no engine to dispatch to, a lane the daemon's substrate cannot place must stay refused")
	}
	if !strings.Contains(diagnostic, `stage "build"`) {
		t.Errorf("refusal must keep the solver's named diagnostic: %s", diagnostic)
	}
	if _, deferred := decisions.EngineDeferred[seamIdentity]; deferred {
		t.Error("an engine-disabled lane is refused, never deferred")
	}
}

// TestMixedAndSelfPinnedLanesKeepTodaysRefusal covers the two runner-driven
// shapes. A lane with even ONE self-pinned stage is not engine-selected
// (selectEngineForEntry: a Temporal worker cannot run a self-pinned stage),
// so its refusal must be untouched by #3987 — including the mixed case, where
// the exemption would be most tempting and most wrong: the daemon would run
// the self stage in-process and have nowhere to run the pinned one.
func TestMixedAndSelfPinnedLanesKeepTodaysRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  workflow.Definition
	}{
		{
			name: "mixed",
			def: seamDefinition(
				seamTask("build", true, &apiv1.RunsOn{OS: "windows"}, "verify"),
				seamTask("verify", false, nil, ""),
			),
		},
		{
			name: "self-only-stage-pinned-remote",
			def:  seamDefinition(seamTask("build", false, &apiv1.RunsOn{OS: "windows"}, "")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decisions := seamDecisions(t, seamConfig(true), tc.def)
			if _, refused := decisions.Refusals[seamIdentity]; !refused {
				t.Fatalf("a runner-driven lane must keep today's refusal; deferrals=%v", decisions.EngineDeferred)
			}
		})
	}
}

// TestFullInventoryUnplaceableLaneStillRefuses is the other boundary: the
// exemption is earned by being PLACEABLE on the full inventory, not by
// declaring runsOn. A stage no declared runner can satisfy fails
// bootstrap.PinStagePlacements, is therefore not engine-selected, and must
// still be refused — otherwise a typo'd runsOn would silently produce a lane
// that ticks forever and can never dispatch.
func TestFullInventoryUnplaceableLaneStillRefuses(t *testing.T) {
	def := seamDefinition(seamTask("build", true, &apiv1.RunsOn{OS: "macOS"}, ""))
	decisions := seamDecisions(t, seamConfig(true), def)

	diagnostic, refused := decisions.Refusals[seamIdentity]
	if !refused {
		t.Fatal("a stage no declared runner satisfies must still be refused at boot")
	}
	if !strings.Contains(diagnostic, `stage "build"`) {
		t.Errorf("refusal must keep the solver's named diagnostic: %s", diagnostic)
	}
}

// TestPlacementRefusalWithoutSelectionsRefusesEverything is the fail-closed
// default: with no selection information at all (a nil map), every
// unsatisfiable lane is refused. The carve-out must be something a caller
// opts into by proving engine selection, never the absence of an answer.
func TestPlacementRefusalWithoutSelectionsRefusesEverything(t *testing.T) {
	decisions, err := placementRefusals(seamConfig(true), seamSet(), nil, seamMachines(podPinnedDefinition()), nil)
	if err != nil {
		t.Fatalf("placementRefusals: %v", err)
	}
	if _, refused := decisions.Refusals[seamIdentity]; !refused {
		t.Fatal("with no engine selection proven, checkpoint 3 must fail closed")
	}
}

// TestPodPinnedLaneServesOnEngineEnabledDaemon is the integration half: the
// real daemon boot path, the real config loader, the real solver — the
// reproduction from #3987 run offline.
//
// It asserts the whole chain the outage broke: the entry carries no refusal,
// the scheduler journals no workflow.refused for it, and an explicit trigger
// is no longer rejected as placement-unsatisfiable but reaches ENGINE
// DISPATCH (this test daemon has no Temporal client, so the dispatch fails
// there — which is precisely the proof that the run got past checkpoint 3
// and into the engine lane it belongs in).
func TestPodPinnedLaneServesOnEngineEnabledDaemon(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	declareRemoteRunner(t, root, "  - name: ci\n    host: ghcr.io/example/ci:v1\n    provides:\n      os: windows\n")
	writeSecondWorkflow(t, root, remoteOnlyV30WorkflowYAML)

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), instance.NewLayout(root), &wg)
	if err != nil {
		t.Fatalf("boot must never kill (#2860): %v", err)
	}
	defer func() { _ = setup.Shutdown(context.Background()) }()

	var pinned *localscheduler.WorkflowEntry
	for i := range setup.Entries {
		if setup.Entries[i].Workflow == "win-build" {
			pinned = &setup.Entries[i]
		}
	}
	if pinned == nil {
		t.Fatalf("win-build missing from entries: %+v", setup.Entries)
	}
	if pinned.PlacementRefusal != "" {
		t.Fatalf("a fully pod-pinned lane on an engine-enabled instance must not be boot-refused (#3987): %s", pinned.PlacementRefusal)
	}
	if _, ok := pinned.Starter.(*engineStarter); !ok {
		t.Fatalf("the exempted lane must be the one the engine drives, got %T", pinned.Starter)
	}

	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	runID, err := sched.Trigger(context.Background(), "win-build", time.Now())
	if err != nil {
		var rejected *localscheduler.TriggerRejectedError
		if errors.As(err, &rejected) && strings.HasPrefix(rejected.Reason, localscheduler.ReasonPlacementUnsatisfiable) {
			t.Fatalf("the trigger must not be refused as unplaceable (#3987): %q", rejected.Reason)
		}
		t.Fatalf("the trigger must be admitted, got %v", err)
	}
	if runID == "" {
		t.Fatal("an admitted trigger must mint a run id")
	}

	// The run reaches ENGINE DISPATCH, which is the proof it left checkpoint
	// 3 behind: this test daemon has no Temporal client, so the dispatch
	// fails there and the failure is journaled against the run.
	deadline := time.Now().Add(10 * time.Second)
	var dispatched bool
	for time.Now().Before(deadline) && !dispatched {
		events, err := journal.ReadInstanceLog(setup.InstanceLog.Dir())
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			if ev.Type == journal.EventWorkflowRefused && ev.Workflow == "win-build" {
				t.Fatalf("an engine-selected lane must journal no workflow.refused: %+v", ev)
			}
			if ev.RunID == runID && strings.Contains(ev.Status, errEngineRuntimeUnattached.Error()) {
				dispatched = true
			}
		}
		if !dispatched {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !dispatched {
		t.Fatal("the admitted run must reach engine dispatch rather than the local runner")
	}
}
