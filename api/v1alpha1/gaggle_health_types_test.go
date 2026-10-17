package v1alpha1

import (
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
