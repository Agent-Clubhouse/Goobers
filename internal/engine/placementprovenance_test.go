package engine

// placementprovenance_test.go covers plan item E3's engine half (#3875): the
// per-attempt runner.placement event dispatchWithRetry journals from what the
// dispatch reported back.
//
// The runner-vs-engine agreement is the parity harness's row
// (parity_row_placement_provenance_test.go). These are the properties that row
// cannot reach: the POD arm's provenance (the parity harness has no pod), the
// zero-declaration gate, and the replay contract for histories recorded before
// the result field existed.

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// projectedPlacements decodes every runner.placement event out of a run's
// projection, keyed "<stage>#<attempt>".
func projectedPlacements(t *testing.T, proj JournalProjection) map[string]journal.Placement {
	t.Helper()
	out := map[string]journal.Placement{}
	for _, op := range proj.Ops {
		if op.Kind != opAppend || op.Event == nil {
			continue
		}
		if placement, ok := journal.PlacementFromEvent(*op.Event); ok {
			out[op.Event.Stage+"#"+strconv.Itoa(op.Event.Attempt)] = placement
		}
	}
	return out
}

// THE HEADLINE FOR THE POD ARM: the substrate facts the dispatcher reports —
// which runner served the stage, which pod carried it, which image that pod ran,
// and the queue-wait bounds — reach events.jsonl. Before E3 they crossed the
// activity boundary on DispatchStageResult.Placement and were dropped on the
// floor, so §11 acceptance 6 had no evidence behind it.
func TestPodAttemptJournalsDispatcherPlacementProvenance(t *testing.T) {
	queuedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	podStartedAt := queuedAt.Add(42 * time.Second)

	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "build",
		Tasks: []apiv1.Task{podTask("build", "", nil)},
	}
	in := runInput("pod-placement", spec)
	in.Placements = []PinnedPlacement{remotePin("build")}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "linux-toolchain-ci", Pod: "goobers-stage-build-1", Image: "ghcr.io/example/stage:v9",
		QueuedAt: queuedAt, PodStartedAt: podStartedAt,
		Phase: corev1.PodSucceeded, SurrenderConfirmed: true,
	}}

	proj := executeForProjection(t, in, &Activities{
		Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders,
	}, false)

	placements := projectedPlacements(t, proj)
	got, ok := placements["build#1"]
	if !ok {
		t.Fatalf("no %s event for the pod attempt; the stall sweep and §11 acceptance 6 have nothing to read", journal.EventRunnerPlacement)
	}
	if got.Runner != "linux-toolchain-ci" || got.Pod != "goobers-stage-build-1" || got.Image != "ghcr.io/example/stage:v9" {
		t.Fatalf("placement = %+v, want the dispatcher's runner/pod/image", got)
	}
	if got.QueuedAt == nil || !got.QueuedAt.Equal(queuedAt) {
		t.Fatalf("queuedAt = %v, want %v — the dispatch-latency carrier the scale rung reads", got.QueuedAt, queuedAt)
	}
	if got.PodStartedAt == nil || !got.PodStartedAt.Equal(podStartedAt) {
		t.Fatalf("podStartedAt = %v, want %v", got.PodStartedAt, podStartedAt)
	}
}

// A pod attempt's placement rides its own arm, so it must be journaled whether
// or not the deployment set GOOBERS_RUNNER_* on the WORKER: the pod's substrate
// is reported by the dispatcher that created it, and a dispatch only happens on
// an instance that declared a runners inventory in the first place.
func TestPodPlacementProvenanceIsNotGatedOnWorkerEnvironment(t *testing.T) {
	for _, envVar := range []string{runner.EnvPlacementNode, runner.EnvPlacementPod, runner.EnvPlacementImage} {
		t.Setenv(envVar, "")
	}
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "build",
		Tasks: []apiv1.Task{podTask("build", "", nil)},
	}
	in := runInput("pod-placement-undeclared", spec)
	in.Placements = []PinnedPlacement{remotePin("build")}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "linux-cli", Pod: "p", Image: "i", Phase: corev1.PodSucceeded, SurrenderConfirmed: true,
	}}

	proj := executeForProjection(t, in, &Activities{
		Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders,
	}, false)

	if _, ok := projectedPlacements(t, proj)["build#1"]; !ok {
		t.Fatal("a pod attempt journaled no placement; the worker's own env family does not gate the pod's provenance")
	}
}

// Zero-declaration invariance (goobernetes-architecture.md §11 item 1) on the
// engine path: an install that declares no runners and sets no GOOBERS_RUNNER_*
// keeps producing the journals it produced before E3 existed. The local runner
// has had this gate since placement provenance landed; the engine must not be
// the path that quietly breaks it.
func TestEnginePlacementProvenanceRespectsZeroDeclaration(t *testing.T) {
	for _, envVar := range []string{runner.EnvPlacementNode, runner.EnvPlacementPod, runner.EnvPlacementImage} {
		t.Setenv(envVar, "")
	}
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	proj := executeForProjection(t, projectionInput("placement-undeclared", spec), &Activities{
		Det: &scriptedStages{}, Workspaces: testWorkspaces(t),
	}, false)

	if placements := projectedPlacements(t, proj); len(placements) != 0 {
		t.Fatalf("an undeclared install journaled %d placement event(s) %v; §11 item 1 requires byte-identical journals", len(placements), placements)
	}
}

// The other half of the same gate: once the deployment declares placement, the
// in-process arm journals what runner.SelfPlacement observes — the SAME payload
// the local runner writes, from the same producer.
func TestDeclaredSelfPlacementIsJournaledByTheInProcessArm(t *testing.T) {
	t.Setenv(runner.EnvPlacementNode, "node-a")
	t.Setenv(runner.EnvPlacementPod, "goobers-worker-0")
	t.Setenv(runner.EnvPlacementImage, "ghcr.io/example/worker:v3")

	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	proj := executeForProjection(t, projectionInput("placement-declared", spec), &Activities{
		Det: &scriptedStages{}, Workspaces: testWorkspaces(t),
	}, false)

	got, ok := projectedPlacements(t, proj)["implement#1"]
	if !ok {
		t.Fatal("a declared install journaled no placement for the in-process arm")
	}
	want := runner.SelfPlacement()
	if got.Runner != want.Runner || got.Node != want.Node || got.Pod != want.Pod || got.Image != want.Image || got.OS != want.OS || got.Host != want.Host {
		t.Fatalf("self placement = %+v, want runner.SelfPlacement() = %+v — the two runners must journal one payload from one producer", got, want)
	}
	if got.QueuedAt != nil || got.PodStartedAt != nil {
		t.Fatalf("self placement carries queue timestamps %+v; a self attempt never queued", got)
	}
}

// REPLAY CONTRACT. DispatchStageResult.SelfPlacement is additive and omitempty,
// so an activity result recorded by a build that predates it decodes with a nil
// SelfPlacement and journals nothing — which is exactly what that history
// projected before, byte for byte.
//
// Asserted at the decode boundary rather than through a recorded fixture
// because this is precisely the property a fixture cannot outlive: the payload
// under test is "the JSON the old build wrote".
func TestPreE3ActivityResultDecodesWithoutPlacementAndJournalsNothing(t *testing.T) {
	recorded := []byte(`{"status":"success","summary":"ok","mutations":[{"provider":"github","kind":"pr","id":"1"}]}`)
	var result stageActivityResult
	if err := json.Unmarshal(recorded, &result); err != nil {
		t.Fatalf("a pre-E3 recorded activity result must still decode: %v", err)
	}
	if result.Status != apiv1.ResultSuccess || len(result.Mutations) != 1 {
		t.Fatalf("decoded pre-E3 result lost fields: %+v", result)
	}
	if result.Placement != nil || result.SelfPlacement != nil {
		t.Fatalf("pre-E3 result decoded a placement out of nowhere: %+v", result)
	}
	if _, ok := attemptPlacement(result); ok {
		t.Fatal("a result carrying no placement must journal none; replaying an old history must not invent provenance")
	}
}

// attemptPlacement is the projection every journaled placement goes through, so
// its rules are pinned directly: the pod arm wins, zero timestamps stay ABSENT
// rather than becoming the zero instant, and a block with no runner name is not
// journaled at all.
func TestAttemptPlacementProjection(t *testing.T) {
	queued := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	t.Run("pod arm wins over self", func(t *testing.T) {
		got, ok := attemptPlacement(stageActivityResult{
			Placement:     &StagePlacement{Runner: "linux-ci", Pod: "p", Image: "i", QueuedAt: queued},
			SelfPlacement: &journal.Placement{Runner: journal.PlacementRunnerSelf},
		})
		if !ok || got.Runner != "linux-ci" || got.Pod != "p" {
			t.Fatalf("attemptPlacement = %+v (ok=%t), want the pod that actually ran the work", got, ok)
		}
		if got.QueuedAt == nil || !got.QueuedAt.Equal(queued) {
			t.Fatalf("queuedAt = %v, want %v", got.QueuedAt, queued)
		}
		if got.PodStartedAt != nil {
			t.Fatalf("a zero PodStartedAt must stay absent, got %v", got.PodStartedAt)
		}
	})

	t.Run("self arm when there is no pod", func(t *testing.T) {
		got, ok := attemptPlacement(stageActivityResult{
			SelfPlacement: &journal.Placement{Runner: journal.PlacementRunnerSelf, OS: "linux"},
		})
		if !ok || got.Runner != journal.PlacementRunnerSelf || got.OS != "linux" {
			t.Fatalf("attemptPlacement = %+v (ok=%t), want the self observation", got, ok)
		}
	})

	t.Run("nothing to journal", func(t *testing.T) {
		for name, result := range map[string]stageActivityResult{
			"no placement at all":  {},
			"pod block unnamed":    {Placement: &StagePlacement{Pod: "p"}},
			"self block unnamed":   {SelfPlacement: &journal.Placement{OS: "linux"}},
			"failed dispatch only": {ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultFailure}},
		} {
			if _, ok := attemptPlacement(result); ok {
				t.Errorf("%s: attemptPlacement reported a placement; journal.PlacementFromEvent would refuse it as unnamed", name)
			}
		}
	})
}
