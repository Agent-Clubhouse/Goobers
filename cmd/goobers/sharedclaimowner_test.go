package main

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
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
	layout, run := newPinnedClaimResolverRun(t, "shared")
	instanceID, err := instance.ReadRootIdentity(layout.Root)
	if err != nil {
		t.Fatal(err)
	}
	resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
		return &pinnedClaimTestStore{}, nil
	}}
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	binding, err := resolver.Admission(t.Context(), key, "shared-run", "claim")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Owner.Instance != instanceID || binding.Owner.Run != "shared-run" {
		t.Fatalf("stored ownership: %+v", binding.Owner)
	}
	if _, err := resolver.Admission(t.Context(), key, "shared-run", "other-workflow"); err == nil {
		t.Fatal("request replaced stored workflow identity")
	}
	other := key
	other.Gaggle = "other-gaggle"
	if _, err := resolver.Admission(t.Context(), other, "shared-run", "claim"); err == nil {
		t.Fatal("request replaced stored gaggle identity")
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Admission(t.Context(), key, "shared-run", "claim"); err == nil {
		t.Fatal("terminal run acquired new shared ownership")
	}
}
