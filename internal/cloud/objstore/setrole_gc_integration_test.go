//go:build cloudintegration

package objstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestBlobGCRunsAsControllerRole runs the config prune and the two-observation
// GC sweep through a pool connected as goobers_controller, so the write-path
// inventory's grants (blob_gc_candidate, blob_reference_writer_protocol, and
// EXECUTE on the SECURITY INVOKER reference checks) are exercised, not
// bypassed by a superuser.
func TestBlobGCRunsAsControllerRole(t *testing.T) {
	_, admin, store := testMetadata(t, "goobers-config-gc-controller-role")
	// installBlobReferenceSchema creates stand-ins for reference tables that
	// later PRs add with their own inventory rows. Grant the controller what
	// those rows will: SELECT on the columns blob_is_referenced probes.
	if _, err := admin.Exec(t.Context(), `
		GRANT USAGE ON SCHEMA journal TO goobers_controller;
		GRANT SELECT (digest) ON journal.run_artifact TO goobers_controller;
		GRANT SELECT (actor_digest, payload_digest, proposal_digest, snapshot_receipt_digest,
			snapshot_bundle_digest, result_receipt_digest, result_bundle_digest, result_ref, workspace_ref)
			ON control.child_lineage TO goobers_controller;
		GRANT SELECT (config_digest, policy_digest) ON control.child_authority TO goobers_controller;
		GRANT SELECT (digest) ON control.child_blob TO goobers_controller;
		GRANT SELECT (contract_digest, digest) ON control.child_blob_read TO goobers_controller;
		GRANT SELECT (intent_digest, receipt_digest) ON control.child_publication TO goobers_controller;
		GRANT SELECT (request_digest, previous_digest, plan_digest, result_ref) ON control.child_disposition TO goobers_controller;
		GRANT SELECT (request_digest, previous_digest, plan_digest, result_ref) ON control.child_disposition_history TO goobers_controller`); err != nil {
		t.Fatal(err)
	}
	config := admin.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	controller, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	metadata, err := NewMetadataWithOptions(controller, store, MetadataOptions{
		ExperimentalPhysicalDelete: true,
		MaxPublishLatency:          time.Hour,
		PITRRetention:              DefaultPITRRetention,
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte("controller-role config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatalf("RecordGeneration as goobers_controller: %v", err)
	}
	if err := metadata.PruneGeneration(t.Context(), "instance", digest); err != nil {
		t.Fatalf("PruneGeneration as goobers_controller: %v", err)
	}
	ageCandidate(t, admin, digest)
	manager, handle := gcLease(t, controller)
	for index := 0; index < 2; index++ {
		if deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour); err != nil || deleted != 0 {
			t.Fatalf("observing sweep %d as goobers_controller = %d, %v", index+1, deleted, err)
		}
	}
	ageObservation(t, admin, digest)
	deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
	if err != nil || deleted != 1 {
		t.Fatalf("mature sweep as goobers_controller = %d, %v; want 1", deleted, err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || has {
		t.Fatalf("blob after controller-role sweep Has=%v err=%v", has, err)
	}
}
