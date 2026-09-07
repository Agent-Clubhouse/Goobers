package readservice

import (
	"testing"
	"time"
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
