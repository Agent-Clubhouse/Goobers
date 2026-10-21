package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

func TestExpiredSnapshotCannotReleaseRenewedSharedClaim(t *testing.T) {
	layout, _ := newPinnedClaimResolverRun(t, "shared")
	store := &pinnedClaimTestStore{}
	resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) { return store, nil }}
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	binding, err := resolver.Admission(t.Context(), key, "shared-run", "claim")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName), localscheduler.WithLedgerClock(func() time.Time { return old }))
	if err != nil {
		t.Fatal(err)
	}
	reserved, ok, _, err := ledger.BeginSharedAdmission(key, binding.Owner, "claim")
	if err != nil || !ok {
		t.Fatalf("reserve %v %v", ok, err)
	}
	expired := old.Add(30 * time.Second)
	if ok, _, err := ledger.FinishSharedAdmission(reserved, expired); err != nil || !ok {
		t.Fatalf("finish %v %v", ok, err)
	}
	store.record = sharedclaim.Record{Version: 1, Owner: binding.Owner, ExpiresAt: expired}
	store.revision = "seed"
	// Recovery scans an actually expired persisted lease, then releases its lock.
	snapshot := ledger.Snapshot()
	now := time.Now()
	if len(snapshot) != 1 || snapshot[0].ExpiresAt.After(now) {
		t.Fatal("bad expired snapshot")
	}
	client, err := fileClaimLedgerWithResolver(layout, resolver)
	if err != nil {
		t.Fatal(err)
	}
	// Same owner successfully reacquires before recovery obtains its item lock.
	if ok, _, err := client.ClaimScoped(t.Context(), key, "shared-run", "claim", time.Minute); err != nil || !ok {
		t.Fatalf("reacquire %v %v", ok, err)
	}
	current, err := client.ForRunAll(t.Context(), "shared-run")
	if err != nil || len(current) != 1 || !current[0].ExpiresAt.After(now) {
		t.Fatalf("renewed %+v %v", current, err)
	}
	concrete, ok := client.(*claimsclient.File)
	if !ok {
		t.Fatalf("ledger type %T", client)
	}
	writes := store.writes
	released, err := recoverExpiredSharedClaims(t.Context(), snapshot, concrete, now)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := client.ForRunAll(t.Context(), "shared-run")
	if err != nil || len(released) != 0 || len(remaining) != 1 || store.record.Owner != binding.Owner {
		t.Fatalf("stale expiry scan released renewed lease: released=%d remaining=%d owner=%+v err=%v", len(released), len(remaining), store.record.Owner, err)
	}
	if remaining[0].ReleasePending || !remaining[0].ExpiresAt.Equal(current[0].ExpiresAt) || store.writes != writes {
		t.Fatal("stale expiry scan changed renewed custody or the provider record")
	}
}
