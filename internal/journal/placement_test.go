package journal

import (
	"encoding/json"
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
		Host:         "goobers-stage-implement-4x2vq",
		OS:           "linux",
		Build:        "v0.2.0",
		Worker:       "goobers-worker/v0.2.0@goobers-stage-implement-4x2vq#42",
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
	if got.Runner != want.Runner || got.Node != want.Node || got.Host != want.Host || got.OS != want.OS ||
		got.Build != want.Build || got.Worker != want.Worker || got.Image != want.Image || got.Pod != want.Pod {
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
