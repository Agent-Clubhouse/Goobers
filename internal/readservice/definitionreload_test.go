package readservice

import (
	"testing"
	"time"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/workflowsafety"
)

func TestDefinitionReloadSnapshotsAreIndependent(t *testing.T) {
	s := &Local{}
	if s.definitionReloadSnapshot() != nil {
		t.Fatal("offline service fabricated a reload observation")
	}

	s.PublishDefinitionReload(DefinitionReloadStatus{AppliedDigest: "old", ObservedDigest: "new", ObservedAt: time.Now(), State: "rejected", Watching: true})
	first := s.definitionReloadSnapshot()
	first.AppliedDigest = "tampered"
	if s.definitionReloadSnapshot().AppliedDigest != "old" {
		t.Fatal("reader mutated shared snapshot")
	}
	s.PublishDefinitionReload(DefinitionReloadStatus{AppliedDigest: "new", ObservedDigest: "new", State: "current"})
	if first.State != "rejected" || s.definitionReloadSnapshot().State != "current" {
		t.Fatal("publication mutated an earlier response")
	}
}

func TestSafetyCandidateWarningSnapshotsAreIndependent(t *testing.T) {
	s := &Local{}
	details := &workflowsafety.Details{WitnessPath: []string{"review(fail)", "@escalate"}}
	status := DefinitionReloadStatus{State: "rejected", CandidateWarnings: []validate.CodedWarning{{
		Code: workflowsafety.PublishCode, Explanation: "candidate", Safety: details,
	}}}
	s.PublishDefinitionReload(status)
	details.WitnessPath[0] = "mutated source"
	first := s.definitionReloadSnapshot()
	first.CandidateWarnings[0].Safety.WitnessPath[0] = "mutated reader"
	if got := s.definitionReloadSnapshot().CandidateWarnings[0].Safety.WitnessPath[0]; got != "review(fail)" {
		t.Fatalf("candidate warning snapshot mutated: %q", got)
	}
}
