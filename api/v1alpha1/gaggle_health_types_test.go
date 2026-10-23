package v1alpha1

import (
	"reflect"
	"testing"
	"time"
)

func TestGaggleHealthResponseDeepCopy(t *testing.T) {
	lastSuccessful := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	original := &GaggleHealthResponse{
		Controller: &GaggleHealthControllerStatus{
			LastSuccessfulEvaluation: &lastSuccessful,
			LastError:                "timed out",
		},
	}

	copied := original.DeepCopy()
	copied.Controller.LastError = "detector failed"
	*copied.Controller.LastSuccessfulEvaluation = lastSuccessful.Add(time.Minute)

	if original.Controller.LastError != "timed out" {
		t.Fatalf("copy aliases controller status: %+v", original.Controller)
	}
	if !original.Controller.LastSuccessfulEvaluation.Equal(lastSuccessful) {
		t.Fatalf("copy aliases last successful evaluation: %v", original.Controller.LastSuccessfulEvaluation)
	}
}

func TestGaggleHealthCopiesDetachFindingEvidenceAndTimestamps(t *testing.T) {
	stamp := time.Date(2026, time.October, 6, 12, 0, 0, 123, time.UTC)
	pointedAt := stamp
	finding := GaggleHealthFinding{
		FirstObserved: stamp, LastObserved: stamp,
		Evidence:           []GaggleHealthEvidence{{Detail: "original"}},
		Repair:             GaggleHealthRepair{AttemptedAt: &pointedAt, CompletedAt: &pointedAt},
		ResolvedAt:         &pointedAt,
		ResolutionEvidence: []GaggleHealthEvidence{{Detail: "original"}},
	}
	snapshot := &GaggleHealthSnapshot{
		UpdatedAt: stamp, Active: []GaggleHealthFinding{finding}, History: []GaggleHealthFinding{finding},
	}
	copied := snapshot.DeepCopy()
	if !reflect.DeepEqual(copied, snapshot) {
		t.Fatal("copy changed snapshot values")
	}
	event := &GaggleHealthEvent{OccurredAt: stamp, Finding: &finding}
	eventCopy := event.DeepCopy()
	for _, copy := range []*GaggleHealthFinding{&copied.Active[0], &copied.History[0], eventCopy.Finding} {
		copy.Evidence[0].Detail = "changed"
		copy.ResolutionEvidence[0].Detail = "changed"
		*copy.Repair.AttemptedAt = stamp.Add(time.Minute)
		*copy.Repair.CompletedAt = stamp.Add(time.Minute)
		*copy.ResolvedAt = stamp.Add(time.Minute)
	}
	for _, original := range []*GaggleHealthFinding{&snapshot.Active[0], &snapshot.History[0], event.Finding} {
		if original.Evidence[0].Detail != "original" || original.ResolutionEvidence[0].Detail != "original" ||
			!original.Repair.AttemptedAt.Equal(stamp) || !original.Repair.CompletedAt.Equal(stamp) || !original.ResolvedAt.Equal(stamp) {
			t.Fatalf("copy aliases finding: %+v", original)
		}
	}
}

func TestGaggleHealthCopiesPreserveNilAndEmptyCollections(t *testing.T) {
	for _, original := range []*GaggleHealthSnapshot{
		{},
		{Active: []GaggleHealthFinding{}, History: []GaggleHealthFinding{}},
		{Active: []GaggleHealthFinding{{Evidence: []GaggleHealthEvidence{}, ResolutionEvidence: []GaggleHealthEvidence{}}}},
	} {
		if copied := original.DeepCopy(); !reflect.DeepEqual(copied, original) {
			t.Fatalf("copy changed collection shape: got %#v, want %#v", copied, original)
		}
	}
}
