//go:build cloudintegration

package acceptance

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// A child tombstoned before the engine bound it never reached the bind's
// release of its acceptance's generation pin; retention releases it with the
// tombstone. A bound child's pin is the bind's to release and is untouched.
// The pass is idempotent (#8085 c).
func TestChildTombstoneReleasesAnUnboundChildsGenerationPin(t *testing.T) {
	store, owner := childTestStore(t)
	const generation = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if _, err := owner.Exec(t.Context(), `INSERT INTO control.config_generation (instance_id,digest,blob_digest) VALUES ($1,$2,$2)`,
		store.instanceID, generation); err != nil {
		t.Fatal(err)
	}
	accept := func(occurrence string) triggerqueue.ChildRecord {
		child, _, err := store.AcceptChild(t.Context(), childRequest("parent", occurrence, "call"), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(t.Context(), `INSERT INTO control.config_generation_pin (instance_id,digest,owner) VALUES ($1,$2,$3)`,
			store.instanceID, generation, child.AcceptanceID); err != nil {
			t.Fatal(err)
		}
		finishReviewChild(t, store, child)
		return child
	}
	unbound, bound := accept("stage-unbound"), accept("stage-bound")
	if _, err := owner.Exec(t.Context(), `
		INSERT INTO control.child_engine_start (instance_id,child_id,manifest_digest,parent_workflow,parent_workflow_id,
			parent_temporal_run_id,deadline,owner_epoch)
		VALUES ($1,$2,$3,'parent-workflow','parent-wf','temporal-run',clock_timestamp()+interval '1 hour',$4)`,
		store.instanceID, bound.ChildID, generation, store.handle.Epoch); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkChildParentSettled(t.Context(), unbound.Identity.ChildParent, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `UPDATE control.child_parent SET settled_at=clock_timestamp()-interval '31 days' WHERE instance_id=$1`,
		store.instanceID); err != nil {
		t.Fatal(err)
	}
	pinned := func(child triggerqueue.ChildRecord) bool {
		t.Helper()
		var present bool
		if err := owner.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM control.config_generation_pin WHERE instance_id=$1 AND owner=$2)`,
			store.instanceID, child.AcceptanceID).Scan(&present); err != nil {
			t.Fatal(err)
		}
		return present
	}
	if !pinned(unbound) || !pinned(bound) {
		t.Fatal("fixture pins missing")
	}
	result, err := store.PruneChildren(t.Context(), time.Now(), 100)
	if err != nil || result.Tombstoned != 2 {
		t.Fatalf("prune = %+v, %v; want both children tombstoned", result, err)
	}
	if pinned(unbound) {
		t.Fatal("the unbound tombstoned child's generation pin outlived it")
	}
	if !pinned(bound) {
		t.Fatal("retention released a bound child's pin, which is the bind's to release")
	}
	if _, err := store.PruneChildren(t.Context(), time.Now(), 100); err != nil {
		t.Fatalf("a repeated pass: %v", err)
	}
	if !pinned(bound) {
		t.Fatal("a repeated pass released the bound child's pin")
	}
}
