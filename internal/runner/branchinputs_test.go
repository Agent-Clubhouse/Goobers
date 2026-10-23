package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestBranchInputResolutionPreservesValueAndProvenance(t *testing.T) {
	machine := fanInMachine(t, false)
	completed := NewStageOutputs()
	completed.RecordCompleted(apiv1.Task{Name: "review-security"}, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: apiv1.IntegrityUnapproved, Outputs: map[string]any{"summary": "untrusted", "dotted.key": "dotted"}})
	for _, status := range []journal.BranchStatus{journal.BranchSucceeded, journal.BranchFailed, journal.BranchTimedOut, journal.BranchCancelled, journal.BranchNoOutput, journal.BranchSkipped} {
		t.Run(string(status), func(t *testing.T) {
			sources := map[string]BranchInputSource{"security": {Start: "review-security", Status: status, Completed: completed}}
			for _, ref := range []string{"fan.security.summary", "fan.security.review-security.summary", "fan.security.review-security.dotted.key"} {
				got := ResolveBranchInput(ref, machine, "fan", sources)
				if !got.Recognized {
					t.Fatalf("reference not recognized: %s", ref)
				}
				if status != journal.BranchSucceeded {
					if !got.Absent || got.Found || got.Value != nil || got.Integrity != "" {
						t.Fatalf("absent branch leaked content or grade: %+v", got)
					}
					continue
				}
				want := "untrusted"
				if ref == "fan.security.review-security.dotted.key" {
					want = "dotted"
				}
				if !got.Found || got.Absent || got.Value != want || got.Integrity != apiv1.IntegrityUnapproved {
					t.Fatalf("bound value/grade drift: %+v", got)
				}
			}
		})
	}
}

func TestBranchInputResolutionMissingAndLegacyKeys(t *testing.T) {
	machine := fanInMachine(t, false)
	sources := map[string]BranchInputSource{"security": {Start: "review-security", Status: journal.BranchSucceeded, Completed: NewStageOutputs()}}
	for _, ref := range []string{"fan.unknown.summary", "fan.security.missing", "fan.security.unknown.summary"} {
		got := ResolveBranchInput(ref, machine, "fan", sources)
		if !got.Recognized || got.Absent || got.Found || got.Integrity != "" {
			t.Fatalf("missing output must fail closed: %s %+v", ref, got)
		}
	}
	for _, ref := range []string{"literal.key", "other.security.summary", "fan..summary"} {
		if got := ResolveBranchInput(ref, machine, "fan", sources); got.Recognized {
			t.Fatalf("legacy key treated as branch reference: %s", ref)
		}
	}
}
