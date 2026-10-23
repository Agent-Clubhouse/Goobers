//go:build cloudintegration

package objstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blobstore"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgtest"
)

func TestSurrenderWriteOnce(t *testing.T) {
	metadata, _, _ := testMetadata(t, "goobers-surrender")
	if err := metadata.PutSurrender(t.Context(), "instance", "run", "stage", 1, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PutSurrender(t.Context(), "instance", "run", "stage", 1, []byte("first")); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	if err := metadata.PutSurrender(t.Context(), "instance", "run", "stage", 1, []byte("different")); !errors.Is(err, ErrConflict) {
		t.Fatalf("different replay error = %v, want ErrConflict", err)
	}
}

func TestConfigApplyMonotonic(t *testing.T) {
	metadata, pool, _ := testMetadata(t, "goobers-config-apply")
	configDigest := digestFor([]byte("config"))
	if err := metadata.RecordGeneration(t.Context(), "instance", configDigest, []byte("config")); err != nil {
		t.Fatal(err)
	}
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "instance", Name: "control", Holder: "controller",
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apply := func(generation int64) bool {
		var applied bool
		err := manager.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
			var applyErr error
			applied, applyErr = metadata.ApplyConfig(ctx, tx, "instance", configDigest, generation)
			return applyErr
		})
		if err != nil {
			t.Fatal(err)
		}
		return applied
	}
	if !apply(2) {
		t.Fatal("new generation was not applied")
	}
	if apply(1) {
		t.Fatal("stale generation moved applied config backward")
	}
	var generation int64
	if err := pool.QueryRow(t.Context(), "SELECT generation FROM control.applied_config WHERE instance_id='instance'").Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("generation = %d, want 2", generation)
	}
}

func TestPruneRefusesPinned(t *testing.T) {
	metadata, _, store := testMetadata(t, "goobers-config-prune")
	archive := []byte("config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PinGeneration(t.Context(), "instance", digest, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "instance", digest); !errors.Is(err, ErrPinned) {
		t.Fatalf("Prune error = %v, want ErrPinned", err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("pinned blob Has = %v, %v", has, err)
	}
}

func TestPruneDefersBlobUntilRetentionSweep(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-retention")
	archive := []byte("retained config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}

	if err := metadata.PruneGeneration(t.Context(), "instance", digest); err != nil {
		t.Fatal(err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("prune synchronously removed blob: Has=%v err=%v", has, err)
	}
	manager, handle := gcLease(t, pool)
	deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("GC deleted %d blobs inside retention window", deleted)
	}
}

func TestPhysicalBlobDeleteIsDisabledByDefault(t *testing.T) {
	_, pool, store := testMetadata(t, "goobers-config-gc-disabled")
	metadata, err := NewMetadataWithOptions(pool, store, MetadataOptions{
		PITRRetention: DefaultPITRRetention,
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte("default retained archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "instance", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.blob_gc_candidate
		SET unreferenced_at=clock_timestamp()-interval '40 days'
		WHERE digest=$1`,
		digest,
	); err != nil {
		t.Fatal(err)
	}
	manager, handle := gcLease(t, pool)
	deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("first observation physically deleted %d blobs", deleted)
	}
	ageObservation(t, pool, digest)
	deleted, err = metadata.SweepGC(t.Context(), manager, handle, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("default GC physically deleted %d blobs after eligibility", deleted)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("default-retained blob Has=%v err=%v", has, err)
	}
	var intentID, intentETag string
	if err := pool.QueryRow(t.Context(), `
		SELECT intent_id, intent_etag FROM control.blob_gc_candidate
		WHERE digest=$1`,
		digest,
	).Scan(&intentID, &intentETag); err != nil {
		t.Fatal(err)
	}
	if intentID == "" || intentETag == "" {
		t.Fatalf("eligible default-off candidate intent=%q etag=%q", intentID, intentETag)
	}
}

func TestConfiguredPITRRetentionIsTheDeletionFloor(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-gc-pitr-floor")
	if _, err := NewMetadataWithOptions(pool, store, MetadataOptions{
		PITRRetention: 34 * 24 * time.Hour,
	}); err == nil {
		t.Fatal("PITR retention below the 35-day backup window was accepted")
	}
	archive := []byte("PITR retained archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "instance", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.blob_gc_candidate
		SET unreferenced_at=clock_timestamp()-interval '30 minutes'
		WHERE digest=$1`,
		digest,
	); err != nil {
		t.Fatal(err)
	}
	manager, handle := gcLease(t, pool)
	if deleted, err := metadata.SweepGC(
		t.Context(), manager, handle, time.Minute,
	); err != nil || deleted != 0 {
		t.Fatalf("SweepGC inside PITR floor = %d, %v", deleted, err)
	}
	var observedAt *time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT first_observed_at FROM control.blob_gc_candidate WHERE digest=$1`,
		digest,
	).Scan(&observedAt); err != nil {
		t.Fatal(err)
	}
	if observedAt != nil {
		t.Fatalf("PITR-young candidate was observed at %v", observedAt)
	}
}

func TestBlobReferenceCensusCoversEveryClass(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-gc-census")
	tests := []struct {
		name   string
		insert string
	}{
		{"journal", "INSERT INTO journal.run_artifact (digest) VALUES ($1)"},
		{"recovery", `INSERT INTO control.recovery_record (
			instance_id,repository_key,run_id,snapshot_sha,version,ref,
			base_ref,base_sha,patch_digest,archive_digest,archive_bytes,
			archive_format,bundle_key,identity_unix_ns,retain_until,owner_epoch
		) VALUES (
			'instance','repo','run','snapshot',1,'ref','base','sha',
			$1,'',1,'','recovery/bundle',1,
			clock_timestamp()+interval '1 day',1
		)`},
		{"pending-adoption", "INSERT INTO control.child_blob (digest) VALUES ($1)"},
		{"child-grant", "INSERT INTO control.child_authority (config_digest) VALUES ($1)"},
		{"child-result", "INSERT INTO control.child_lineage (result_ref) VALUES ($1)"},
		{"child-workspace", "INSERT INTO control.child_lineage (workspace_ref) VALUES ($1)"},
		{"disposition-result", "INSERT INTO control.child_disposition (result_ref) VALUES ($1)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := []byte("retained " + test.name)
			digest := digestFor(archive)
			if err := metadata.RecordGeneration(t.Context(), "instance-"+test.name, digest, archive); err != nil {
				t.Fatal(err)
			}
			if err := metadata.PruneGeneration(t.Context(), "instance-"+test.name, digest); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), test.insert, digest); err != nil {
				t.Fatal(err)
			}
			ageCandidate(t, pool, digest)
			manager, handle := gcLeaseWithHolder(t, pool, "gc-"+test.name)
			deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if deleted != 0 {
				t.Fatalf("GC deleted %d %s-referenced blobs", deleted, test.name)
			}
			if has, err := store.Has(t.Context(), digest); err != nil || !has {
				t.Fatalf("%s-referenced blob Has=%v err=%v", test.name, has, err)
			}
			var candidates int
			if err := pool.QueryRow(t.Context(), `
				SELECT count(*) FROM control.blob_gc_candidate WHERE digest=$1`,
				digest,
			).Scan(&candidates); err != nil {
				t.Fatal(err)
			}
			if candidates != 0 {
				t.Fatalf("%s-referenced candidate rows = %d", test.name, candidates)
			}
			if err := manager.Release(t.Context(), handle); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBlobReferenceCensusFailsClosedAndExcludesRecoveryBundleKey(t *testing.T) {
	_, pool, _ := testMetadata(t, "goobers-config-gc-census-closed")
	digest := "sha256:" + strings.Repeat("a", 64)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO control.recovery_record (
			instance_id,repository_key,run_id,snapshot_sha,version,ref,
			base_ref,base_sha,patch_digest,archive_digest,archive_bytes,
			archive_format,bundle_key,identity_unix_ns,retain_until,owner_epoch
		) VALUES (
			'instance','repo','run','snapshot',1,'ref','base','sha',
			'','',1,'',$1,1,clock_timestamp()+interval '1 day',1
		)`,
		digest,
	); err != nil {
		t.Fatal(err)
	}
	var referenced bool
	if err := pool.QueryRow(t.Context(), `
		SELECT control.blob_is_referenced($1)`,
		digest,
	).Scan(&referenced); err != nil {
		t.Fatal(err)
	}
	if referenced {
		t.Fatal("recovery bundle_key was treated as a blobs/sha256 reference")
	}
	if _, err := pool.Exec(t.Context(), `
		DELETE FROM control.blob_reference_writer_protocol
		WHERE writer_class='child-custody'`,
	); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT control.blob_is_referenced($1)`,
		digest,
	).Scan(&referenced); err != nil {
		t.Fatal(err)
	}
	if !referenced {
		t.Fatal("unqualified digest-only child referencer did not fail closed")
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO control.blob_reference_writer_protocol (
			writer_class,protocol_version
		) VALUES ('child-custody',1)`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "DROP TABLE journal.run_artifact"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT control.blob_is_referenced($1)`,
		digest,
	).Scan(&referenced); err != nil {
		t.Fatal(err)
	}
	if !referenced {
		t.Fatal("missing journal.run_artifact made the census fail open")
	}
}

func TestPhysicalBlobDeleteRequiresTwoSeparatedObservations(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-gc-two-sweeps")
	archive := []byte("twice observed config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "instance", digest); err != nil {
		t.Fatal(err)
	}
	ageCandidate(t, pool, digest)
	manager, handle := gcLease(t, pool)
	for index := 0; index < 2; index++ {
		deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if deleted != 0 {
			t.Fatalf("sweep %d deleted without separated observations", index+1)
		}
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("blob before separated observation Has=%v err=%v", has, err)
	}
	ageObservation(t, pool, digest)
	deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("mature second sweep deleted %d blobs, want 1", deleted)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || has {
		t.Fatalf("blob after mature second sweep Has=%v err=%v", has, err)
	}
}

func TestLostDatabaseAuthorityCannotDeleteRepublishedGeneration(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-gc-race")
	archive := []byte("shared config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "old-instance", digest, archive); err != nil {
		t.Fatal(err)
	}

	if err := metadata.PruneGeneration(t.Context(), "old-instance", digest); err != nil {
		t.Fatal(err)
	}
	manager, handle := gcLease(t, pool)
	ageCandidate(t, pool, digest)
	if _, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour); err != nil {
		t.Fatal(err)
	}
	ageObservation(t, pool, digest)
	pausing := &pausingMutationStore{
		Azure: store, removing: make(chan struct{}), resume: make(chan struct{}),
		publishing: make(chan struct{}),
	}
	metadata.blobs = pausing
	sweepDone := make(chan error, 1)
	go func() {
		_, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
		sweepDone <- err
	}()
	select {
	case <-pausing.removing:
	case err := <-sweepDone:
		t.Fatalf("GC failed before conditional remove: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("GC never reached conditional remove")
	}

	if err := manager.Release(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	successorManager, successorHandle := gcLeaseWithHolder(t, pool, "successor")
	defer func() { _ = successorManager.Release(context.Background(), successorHandle) }()
	republishDone := make(chan error, 1)
	go func() {
		if err := metadata.RecordGeneration(t.Context(), "new-instance", digest, archive); err != nil {
			republishDone <- err
			return
		}
		republishDone <- metadata.PinGeneration(t.Context(), "new-instance", digest, "active-run")
	}()
	<-pausing.publishing
	select {
	case err := <-republishDone:
		t.Fatalf("publisher crossed the GC storage lease early: %v", err)
	default:
	}
	close(pausing.resume)
	if err := <-sweepDone; err == nil || !errors.Is(err, cloudlease.ErrStale) {
		t.Fatalf("stale collector error = %v, want ErrStale after safe delete", err)
	}
	if err := <-republishDone; err != nil {
		t.Fatal(err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("republished pinned blob Has=%v err=%v", has, err)
	}
	var pins int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation_pin
		WHERE instance_id='new-instance' AND digest=$1`,
		digest,
	).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if pins != 1 {
		t.Fatalf("new generation pins = %d, want 1", pins)
	}
}

func TestPlainPutCannotCrossCollectorLeaseUnchanged(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-gc-plain-put")
	archive := []byte("journal artifact shared with config")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "old-instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "old-instance", digest); err != nil {
		t.Fatal(err)
	}
	manager, handle := gcLease(t, pool)
	ageCandidate(t, pool, digest)
	if _, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour); err != nil {
		t.Fatal(err)
	}
	ageObservation(t, pool, digest)
	pausing := &pausingMutationStore{
		Azure: store, removing: make(chan struct{}), resume: make(chan struct{}),
		publishing: make(chan struct{}),
	}
	metadata.blobs = pausing
	sweepDone := make(chan error, 1)
	go func() {
		_, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
		sweepDone <- err
	}()
	<-pausing.removing

	putDone := make(chan error, 1)
	go func() { putDone <- store.Put(t.Context(), digest, archive) }()
	select {
	case err := <-putDone:
		t.Fatalf("plain Put crossed the collector lease: %v", err)
	default:
	}
	close(pausing.resume)
	if err := <-sweepDone; err != nil {
		t.Fatal(err)
	}
	if err := <-putDone; err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO journal.run_artifact (digest) VALUES ($1)`,
		digest,
	); err != nil {
		t.Fatal(err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("plain-Put referenced blob Has=%v err=%v", has, err)
	}
}

func TestDigestReferenceTouchCannotCrossCollectorLeaseUnchanged(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-gc-reference-touch")
	archive := []byte("config shared with child authority")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "old-instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "old-instance", digest); err != nil {
		t.Fatal(err)
	}
	manager, handle := gcLease(t, pool)
	ageCandidate(t, pool, digest)
	if _, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour); err != nil {
		t.Fatal(err)
	}
	ageObservation(t, pool, digest)
	pausing := &pausingMutationStore{
		Azure: store, removing: make(chan struct{}), resume: make(chan struct{}),
		publishing: make(chan struct{}),
	}
	metadata.blobs = pausing
	sweepDone := make(chan error, 1)
	go func() {
		_, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
		sweepDone <- err
	}()
	<-pausing.removing

	touchDone := make(chan error, 1)
	go func() {
		touchDone <- blobstore.TouchReference(t.Context(), store, digest)
	}()
	select {
	case err := <-touchDone:
		t.Fatalf("reference touch crossed the collector lease: %v", err)
	default:
	}
	close(pausing.resume)
	if err := <-sweepDone; err != nil {
		t.Fatal(err)
	}
	if err := <-touchDone; !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("reference touch after collection = %v, want ErrNotFound", err)
	}
	var references int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.child_authority
		WHERE config_digest=$1`,
		digest,
	).Scan(&references); err != nil {
		t.Fatal(err)
	}
	if references != 0 {
		t.Fatalf("failed reference touch committed %d authority rows", references)
	}
}

func TestPublicationLeasePreventsTouchBeforeStampRace(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-gc-touch-order")
	archive := []byte("storage lease ordered config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "old-instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "old-instance", digest); err != nil {
		t.Fatal(err)
	}
	manager, handle := gcLease(t, pool)
	ageCandidate(t, pool, digest)
	if _, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour); err != nil {
		t.Fatal(err)
	}
	ageObservation(t, pool, digest)
	pausing := &pausingPublicationStore{
		Azure: store, acquired: make(chan struct{}), resume: make(chan struct{}),
	}
	metadata.blobs = pausing
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- metadata.RecordGeneration(
			t.Context(), "new-instance", digest, archive)
	}()
	<-pausing.acquired
	deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("GC crossed publisher storage lease and deleted %d blobs", deleted)
	}
	close(pausing.resume)
	if err := <-publishDone; err != nil {
		t.Fatal(err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("published blob Has=%v err=%v", has, err)
	}
	var references int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation
		WHERE instance_id='new-instance' AND digest=$1`,
		digest,
	).Scan(&references); err != nil {
		t.Fatal(err)
	}
	if references != 1 {
		t.Fatalf("new generation references = %d, want 1", references)
	}
}

func TestPinMissingBlobCompensatesRow(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-pin-missing")
	archive := []byte("missing pin config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}

	if err := store.Remove(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PinGeneration(t.Context(), "instance", digest, "run"); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("Pin error = %v, want ErrNotFound", err)
	}
	var pins int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation_pin
		WHERE instance_id='instance' AND digest=$1 AND owner='run'`,
		digest,
	).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if pins != 0 {
		t.Fatalf("missing blob retained %d pin rows", pins)
	}
}

func TestAcknowledgedPinSurvivesTouchFailure(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-pin-retry")
	archive := []byte("acknowledged pin archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PinGeneration(t.Context(), "instance", digest, "run"); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected touch failure")
	metadata.blobs = &touchFailStore{Azure: store, err: injected}
	if err := metadata.PinGeneration(t.Context(), "instance", digest, "run"); !errors.Is(err, injected) {
		t.Fatalf("retry pin error = %v, want injected touch failure", err)
	}
	var pins int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation_pin
		WHERE instance_id='instance' AND digest=$1 AND owner='run'`,
		digest,
	).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if pins != 1 {
		t.Fatalf("acknowledged pin rows = %d, want 1", pins)
	}
}

func TestInterruptedPostCommitTouchKeepsBlobAndReference(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-touch-interrupted")
	archive := []byte("post-commit touch archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "old-instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "old-instance", digest); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected post-commit touch failure")
	metadata.blobs = &touchFailStore{Azure: store, err: injected}
	err := metadata.RecordGeneration(t.Context(), "new-instance", digest, archive)
	if !errors.Is(err, injected) {
		t.Fatalf("RecordGeneration error = %v, want injected failure", err)
	}
	var references, candidates int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation
		WHERE instance_id='new-instance' AND digest=$1`,
		digest,
	).Scan(&references); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.blob_gc_candidate WHERE digest=$1`,
		digest,
	).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if references != 1 || candidates != 0 {
		t.Fatalf("references=%d candidates=%d, want 1/0", references, candidates)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("committed blob Has=%v err=%v", has, err)
	}
}

func TestExpiredStorageLeaseCannotCommitPublication(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-lease-expired")
	archive := []byte("expired publication lease archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "old-instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "old-instance", digest); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected lease expiry")
	metadata.blobs = &renewFailStore{Azure: store, err: injected}
	err := metadata.RecordGeneration(t.Context(), "new-instance", digest, archive)
	if !errors.Is(err, injected) {
		t.Fatalf("RecordGeneration error = %v, want lease expiry", err)
	}
	var references, candidates int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation
		WHERE instance_id='new-instance' AND digest=$1`,
		digest,
	).Scan(&references); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.blob_gc_candidate WHERE digest=$1`,
		digest,
	).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if references != 0 || candidates != 1 {
		t.Fatalf("references=%d candidates=%d, want 0/1", references, candidates)
	}
}

func gcLease(t *testing.T, pool *pgxpool.Pool) (*cloudlease.Manager, *cloudlease.Handle) {
	return gcLeaseWithHolder(t, pool, "gc-test")
}

func gcLeaseWithHolder(
	t *testing.T,
	pool *pgxpool.Pool,
	holder string,
) (*cloudlease.Manager, *cloudlease.Handle) {
	t.Helper()
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "instance", Name: "gc", Holder: holder,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return manager, handle
}

func ageCandidate(t *testing.T, pool *pgxpool.Pool, digest string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.blob_gc_candidate
		SET unreferenced_at=clock_timestamp()-interval '40 days'
		WHERE digest=$1`,
		digest,
	); err != nil {
		t.Fatal(err)
	}
}

func ageObservation(t *testing.T, pool *pgxpool.Pool, digest string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.blob_gc_candidate
		SET first_observed_at=clock_timestamp()-interval '2 hours'
		WHERE digest=$1`,
		digest,
	); err != nil {
		t.Fatal(err)
	}
}

type pausingMutationStore struct {
	*Azure
	removing   chan struct{}
	resume     chan struct{}
	publishing chan struct{}
	publish    sync.Once
}

type pausingPublicationStore struct {
	*Azure
	acquired chan struct{}
	resume   chan struct{}
}

type touchFailStore struct {
	*Azure
	err error
}

type renewFailStore struct {
	*Azure
	err error
}

type wrappedMutation struct {
	blobMutation
	touchErr error
	renewErr error
	removing chan struct{}
	resume   chan struct{}
}

func (s *pausingMutationStore) AcquireMutation(
	ctx context.Context,
	digest string,
	data []byte,
) (blobMutation, error) {
	s.publish.Do(func() { close(s.publishing) })
	return s.Azure.AcquireMutation(ctx, digest, data)
}

func (s *pausingMutationStore) TryAcquireMutation(
	ctx context.Context,
	digest string,
) (blobMutation, bool, error) {
	mutation, busy, err := s.Azure.TryAcquireMutation(ctx, digest)
	if mutation != nil {
		mutation = &wrappedMutation{
			blobMutation: mutation, removing: s.removing, resume: s.resume,
		}
	}
	return mutation, busy, err
}

func (s *pausingPublicationStore) AcquireMutation(
	ctx context.Context,
	digest string,
	data []byte,
) (blobMutation, error) {
	mutation, err := s.Azure.AcquireMutation(ctx, digest, data)
	if err != nil {
		return nil, err
	}
	close(s.acquired)
	select {
	case <-ctx.Done():
		_ = mutation.Release(context.Background())
		return nil, ctx.Err()
	case <-s.resume:
	}
	return mutation, nil
}

func (s *touchFailStore) AcquireMutation(
	ctx context.Context,
	digest string,
	data []byte,
) (blobMutation, error) {
	mutation, err := s.Azure.AcquireMutation(ctx, digest, data)
	if err != nil {
		return nil, err
	}
	return &wrappedMutation{blobMutation: mutation, touchErr: s.err}, nil
}

func (s *renewFailStore) AcquireMutation(
	ctx context.Context,
	digest string,
	data []byte,
) (blobMutation, error) {
	mutation, err := s.Azure.AcquireMutation(ctx, digest, data)
	if err != nil {
		return nil, err
	}
	return &wrappedMutation{blobMutation: mutation, renewErr: s.err}, nil
}

func (m *wrappedMutation) Renew(ctx context.Context) error {
	if m.renewErr != nil {
		return m.renewErr
	}
	return m.blobMutation.Renew(ctx)
}

func (m *wrappedMutation) Touch(ctx context.Context) error {
	if m.touchErr != nil {
		return m.touchErr
	}
	return m.blobMutation.Touch(ctx)
}

func (m *wrappedMutation) RemoveIfMatch(
	ctx context.Context,
	etag string,
) (bool, error) {
	if m.removing == nil {
		return m.blobMutation.RemoveIfMatch(ctx, etag)
	}
	close(m.removing)
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-m.resume:
	}
	return m.blobMutation.RemoveIfMatch(ctx, etag)
}

func testMetadata(t *testing.T, container string) (*Metadata, *pgxpool.Pool, *Azure) {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := "object_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(t.Context(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	pgtest.EnsureRoles(t, pool)
	if err := (migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "object-test",
		InstanceID: "migration", Holder: "migration",
	}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	installBlobReferenceSchema(t, pool)
	store := testAzureStore(t, container)
	metadata, err := NewMetadataWithOptions(pool, store, MetadataOptions{
		ExperimentalPhysicalDelete: true,
		MaxPublishLatency:          time.Hour,
		PITRRetention:              DefaultPITRRetention,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	return metadata, pool, store
}

func installBlobReferenceSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `
		CREATE SCHEMA IF NOT EXISTS journal;
		CREATE TABLE IF NOT EXISTS journal.run_artifact (
			digest text NOT NULL
		);
		CREATE TABLE IF NOT EXISTS control.recovery_record (
			patch_digest text NOT NULL DEFAULT '',
			archive_digest text NOT NULL DEFAULT '',
			bundle_key text NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS control.child_lineage (
			actor_digest text NOT NULL DEFAULT '',
			payload_digest text NOT NULL DEFAULT '',
			proposal_digest text NOT NULL DEFAULT '',
			snapshot_receipt_digest text NOT NULL DEFAULT '',
			snapshot_bundle_digest text NOT NULL DEFAULT '',
			result_receipt_digest text NOT NULL DEFAULT '',
			result_bundle_digest text NOT NULL DEFAULT '',
			result_ref text NOT NULL DEFAULT '',
			workspace_ref text NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS control.child_authority (
			config_digest text NOT NULL DEFAULT '',
			policy_digest text NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS control.child_blob (
			digest text NOT NULL
		);
		CREATE TABLE IF NOT EXISTS control.child_blob_read (
			contract_digest text NOT NULL DEFAULT '',
			digest text NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS control.child_publication (
			intent_digest text NOT NULL DEFAULT '',
			receipt_digest text NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS control.child_disposition (
			request_digest text NOT NULL DEFAULT '',
			previous_digest text NOT NULL DEFAULT '',
			plan_digest text NOT NULL DEFAULT '',
			result_ref text NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS control.child_disposition_history (
			request_digest text NOT NULL DEFAULT '',
			previous_digest text NOT NULL DEFAULT '',
			plan_digest text NOT NULL DEFAULT '',
			result_ref text NOT NULL DEFAULT ''
		);
		INSERT INTO control.blob_reference_writer_protocol (
			writer_class, protocol_version
		) VALUES ('journal',1),('recovery',1),('child-custody',1)
		ON CONFLICT (writer_class) DO UPDATE
		SET protocol_version=excluded.protocol_version`)
	if err != nil {
		t.Fatal(err)
	}
}
