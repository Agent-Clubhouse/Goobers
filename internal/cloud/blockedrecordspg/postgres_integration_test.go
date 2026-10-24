//go:build cloudintegration

package blockedrecordspg_test

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/providers"
)

var repo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

func block(item string, blockers ...string) parking.Block {
	return parking.Block{Repository: repo, ItemID: item, Blockers: blockers, RunID: "run-" + item, Stage: "implement", Reason: "waits"}
}

func TestACycleIsDetectedAcrossLeaseEpochs(t *testing.T) {
	pool := pgtest.Pool(t, "brpg")
	oldManager := pgtest.Manager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	oldStore := blockedrecordspg.Store{Fence: oldManager}
	if cycle, err := oldStore.RecordBlock(t.Context(), old, block("1", "2")); err != nil || len(cycle.Affected) != 0 {
		t.Fatalf("first block: %+v, %v; want no cycle", cycle, err)
	}
	// The block that closes the cycle is recorded by the successor leader: the
	// predecessor's record must have survived the lease change.
	newManager := pgtest.Manager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	newStore := blockedrecordspg.Store{Fence: newManager}
	cycle, err := newStore.RecordBlock(t.Context(), successor, block("2", "pr/1"))
	if err != nil || len(cycle.Affected) != 2 {
		t.Fatalf("closing block: %+v, %v; want a 2-member cycle", cycle, err)
	}
	// A stale leader can neither record nor read the graph.
	if _, err := oldStore.RecordBlock(t.Context(), old, block("3", "4")); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale leader RecordBlock = %v, want ErrStale", err)
	}
	// Re-recording an item replaces its record.
	if cycle, err := newStore.RecordBlock(t.Context(), successor, block("2", "9")); err != nil || len(cycle.Affected) != 0 {
		t.Fatalf("replaced block still reports a cycle: %+v, %v", cycle, err)
	}
}

func TestBlockedRecordWritesRunAsTheControllerRole(t *testing.T) {
	pool := pgtest.Pool(t, "brpg")
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), "SET ROLE goobers_controller"); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(t.Context(), "RESET ROLE") //nolint:errcheck
	for i := range 2 {
		if _, err := conn.Exec(t.Context(), `
			INSERT INTO control.blocked_record (instance_id, record_key, record, owner_epoch) VALUES ('i', 'k', '{}', $1)
			ON CONFLICT (instance_id, record_key) DO UPDATE
			SET record = EXCLUDED.record, owner_epoch = EXCLUDED.owner_epoch, recorded_at = now()`, i+1); err != nil {
			t.Fatalf("controller upsert %d: %v", i, err)
		}
	}
	if _, err := conn.Exec(t.Context(), `SELECT record_key, record FROM control.blocked_record WHERE instance_id='i' ORDER BY record_key`); err != nil {
		t.Fatalf("controller read: %v", err)
	}
	if _, err := conn.Exec(t.Context(), `DELETE FROM control.blocked_record`); err == nil {
		t.Fatal("controller role may delete blocked records; the grant is wider than the reviewed write path")
	}
}
