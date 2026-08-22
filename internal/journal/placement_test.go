package journal

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestPlacementEventRoundTrip proves the typed payload survives the journal's
// actual wire format: PlacementEvent → events.jsonl JSON → PlacementFromEvent
// yields the payload that was written, including the RFC3339Nano timestamps
// the read model serves as dispatch-latency carriers (smoke §6.3).
func TestPlacementEventRoundTrip(t *testing.T) {
	queuedAt := time.Date(2026, 8, 22, 10, 0, 0, 123456789, time.UTC)
	podStartedAt := queuedAt.Add(7 * time.Second)
	want := Placement{
		Runner:       "linux-large",
		Node:         "aks-linux-0001",
		OS:           "linux",
		Image:        "ghcr.io/goobers/goobers-base:v0.2.0",
		Pod:          "goobers-stage-implement-4x2vq",
		QueuedAt:     &queuedAt,
		PodStartedAt: &podStartedAt,
	}

	event := PlacementEvent("implement", 2, AttemptPolicy, want)
	if event.Type != EventRunnerPlacement || event.Stage != "implement" || event.Attempt != 2 || event.AttemptClass != AttemptPolicy {
		t.Fatalf("event identity = %s/%s/%d/%s, want runner.placement/implement/2/policy",
			event.Type, event.Stage, event.Attempt, event.AttemptClass)
	}

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Event
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	got, ok := PlacementFromEvent(decoded)
	if !ok {
		t.Fatal("PlacementFromEvent reported not-a-placement for a round-tripped placement event")
	}
	if got.Runner != want.Runner || got.Node != want.Node || got.OS != want.OS ||
		got.Image != want.Image || got.Pod != want.Pod {
		t.Fatalf("decoded identity = %+v, want %+v", got, want)
	}
	if got.QueuedAt == nil || !got.QueuedAt.Equal(queuedAt) {
		t.Fatalf("decoded QueuedAt = %v, want %v", got.QueuedAt, queuedAt)
	}
	if got.PodStartedAt == nil || !got.PodStartedAt.Equal(podStartedAt) {
		t.Fatalf("decoded PodStartedAt = %v, want %v", got.PodStartedAt, podStartedAt)
	}
}

// TestPlacementFromEventLenient: readers must not trip over other event
// types, an absent payload, or a payload missing the one required field.
func TestPlacementFromEventLenient(t *testing.T) {
	for name, event := range map[string]Event{
		"wrong type":     {Type: EventRunnerAnnotation, Runner: map[string]any{"runner": "self"}},
		"no payload":     {Type: EventRunnerPlacement},
		"missing runner": {Type: EventRunnerPlacement, Runner: map[string]any{"os": "linux"}},
	} {
		if _, ok := PlacementFromEvent(event); ok {
			t.Errorf("%s: PlacementFromEvent ok = true, want false", name)
		}
	}
	minimal := PlacementEvent("implement", 1, "", Placement{Runner: PlacementRunnerSelf})
	got, ok := PlacementFromEvent(minimal)
	if !ok || got.Runner != PlacementRunnerSelf {
		t.Fatalf("minimal self placement = %+v ok=%t, want runner=self ok=true", got, ok)
	}
	if got.QueuedAt != nil || got.PodStartedAt != nil {
		t.Fatalf("minimal placement carries timestamps %v/%v, want none (a self attempt never queued)", got.QueuedAt, got.PodStartedAt)
	}
}

// TestConformanceViewUnchangedByPlacementEvents is the acceptance proof for
// architecture §11 item 2's runner.* half: a run WITH placement provenance has
// an IDENTICAL conformance view to the same run without it — the A2 diff
// excludes runner.* entirely, so mode-3 provenance can never split the
// conformance surface (D14).
func TestConformanceViewUnchangedByPlacementEvents(t *testing.T) {
	queuedAt := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	base := []Event{
		{Schema: EventSchema, Seq: 1, Type: EventRunStarted},
		{Schema: EventSchema, Seq: 2, Type: EventStageStarted, Stage: "implement", Attempt: 1},
		{Schema: EventSchema, Seq: 3, Type: EventStageFinished, Stage: "implement", Attempt: 1, Status: "success"},
		{Schema: EventSchema, Seq: 4, Type: EventGateEvaluated, Gate: "review", Verdict: "pass", Target: TargetComplete},
		{Schema: EventSchema, Seq: 5, Type: EventRunFinished, Status: "completed"},
	}
	withPlacement := make([]Event, 0, len(base)+2)
	for _, event := range base {
		withPlacement = append(withPlacement, event)
		if event.Type == EventStageStarted {
			placement := PlacementEvent(event.Stage, event.Attempt, event.AttemptClass, Placement{
				Runner:       "linux-large",
				Node:         "aks-linux-0001",
				OS:           "linux",
				Image:        "ghcr.io/goobers/goobers-base:v0.2.0",
				Pod:          "goobers-stage-implement-4x2vq",
				QueuedAt:     &queuedAt,
				PodStartedAt: &queuedAt,
			})
			placement.Schema = EventSchema
			withPlacement = append(withPlacement, placement)
		}
	}
	if len(withPlacement) == len(base) {
		t.Fatal("fixture emitted no placement events")
	}

	if !reflect.DeepEqual(ConformanceView(base), ConformanceView(withPlacement)) {
		t.Fatalf("conformance view differs with placement events present:\nwithout: %v\nwith:    %v",
			ConformanceView(base), ConformanceView(withPlacement))
	}
	if !reflect.DeepEqual(ConformanceBranches(base), ConformanceBranches(withPlacement)) {
		t.Fatal("per-branch conformance view differs with placement events present")
	}
	for _, event := range withPlacement {
		if event.Type == EventRunnerPlacement && event.IsConformanceNormative() {
			t.Fatal("runner.placement reports conformance-normative; it must always be excluded (D14)")
		}
	}
}
