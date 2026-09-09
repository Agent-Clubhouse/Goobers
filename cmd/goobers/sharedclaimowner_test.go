package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestSharedClaimOwnerBindsPersistedRunIncarnation(t *testing.T) {
	identity := journal.RunIdentity{RunID: "run", Gaggle: "gaggle", Workflow: "implement", WorkflowDigest: "sha256:definition", StartedAt: time.Now().UTC()}
	owner, err := sharedClaimOwnerFromIdentity("instance", identity, "repository/42")
	if err != nil {
		t.Fatal(err)
	}
	equivalent := identity
	equivalent.StartedAt = identity.StartedAt.In(time.FixedZone("other", 3600))
	if got, err := sharedClaimOwnerFromIdentity("instance", equivalent, "repository/42"); err != nil || got != owner {
		t.Fatalf("restart changed incarnation: %+v %v", got, err)
	}
	for _, mutate := range []func(*journal.RunIdentity){
		func(id *journal.RunIdentity) { id.StartedAt = id.StartedAt.Add(time.Nanosecond) },
		func(id *journal.RunIdentity) { id.WorkflowDigest = "sha256:other" },
		func(id *journal.RunIdentity) { id.Gaggle = "other" },
	} {
		changed := identity
		mutate(&changed)
		if got, err := sharedClaimOwnerFromIdentity("instance", changed, "repository/42"); err != nil || got == owner {
			t.Fatalf("different incarnation reused owner: %+v %v", got, err)
		}
	}
	if got, err := sharedClaimOwnerFromIdentity("instance", identity, "repository/43"); err != nil || got == owner {
		t.Fatal("different item reused incarnation")
	}
	identity.StartedAt = time.Time{}
	if _, err := sharedClaimOwnerFromIdentity("instance", identity, "repository/42"); err == nil {
		t.Fatal("missing durable incarnation accepted")
	}
}

func TestSharedClaimOwnerResolvesStoredOwnership(t *testing.T) {
	layout := instance.NewLayout(initDemo(t))
	instanceID, err := instance.ReadRootIdentity(layout.Root)
	if err != nil {
		t.Fatal(err)
	}
	identity := journal.RunIdentity{RunID: "shared-owner", Gaggle: "gaggle", Workflow: "implement", WorkflowVersion: 1, WorkflowDigest: "sha256:definition", StartedAt: time.Now().UTC()}
	run, err := journal.Create(layout.RunsDir(), identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	owner, err := resolveSharedClaimOwner(t.Context(), layout, identity.RunID, identity.Workflow, identity.Gaggle, "repository/42")
	if err != nil || owner.Instance != instanceID || owner.Run != identity.RunID {
		t.Fatalf("stored ownership: %+v %v", owner, err)
	}
	if _, err := resolveSharedClaimOwner(t.Context(), layout, identity.RunID, "other-workflow", identity.Gaggle, "repository/42"); err == nil {
		t.Fatal("request replaced stored workflow identity")
	}
	if _, err := resolveSharedClaimOwner(t.Context(), layout, identity.RunID, identity.Workflow, "other-gaggle", "repository/42"); err == nil {
		t.Fatal("request replaced stored gaggle identity")
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSharedClaimOwner(t.Context(), layout, identity.RunID, identity.Workflow, identity.Gaggle, "repository/42"); err == nil {
		t.Fatal("terminal run acquired new shared ownership")
	}
}
