package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestParentArchiveCleanupRequiresAcknowledgedChildCustody(t *testing.T) {
	f := newChildDrainFixture(t)
	layout := f.launcher.layout
	child := f.submission.Child
	id := journal.RunIdentity{RunID: child.Identity.ParentRunID, Gaggle: child.Identity.Gaggle}
	if err := verifyParentArchiveChildren(t.Context(), layout, id); err == nil {
		t.Fatal("missing custody service permitted cleanup")
	}
	service := &daemonCredentialService{childQueue: f.service.queue}
	registerStageGrantMinter(layout.Root, service)
	t.Cleanup(func() { unregisterDaemonStageGrants(layout.Root, service) })
	if err := verifyParentArchiveChildren(t.Context(), layout, id); err == nil {
		t.Fatal("queued child permitted parent cleanup")
	}
	now := time.Now()
	if err := f.service.queue.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildCancelled, ResultRef: "retained-result"}, now); err != nil {
		t.Fatal(err)
	}
	if err := verifyParentArchiveChildren(t.Context(), layout, id); err == nil {
		t.Fatal("unacknowledged terminal child permitted cleanup")
	}
	if err := f.service.queue.AcknowledgeChild(t.Context(), child.Identity, "retained-result", now); err != nil {
		t.Fatal(err)
	}
	if err := verifyParentArchiveChildren(t.Context(), layout, id); err != nil {
		t.Fatal("acknowledged family blocked cleanup", err)
	}
}
