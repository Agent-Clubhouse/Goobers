package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

// seedLiveRecoveryRecord publishes one live (non-retired) recovery record
// owned by runID and returns its inventory directory, so a caller can retire
// it with the same ".retired-<digest>" rename RetireSnapshot itself performs.
func seedLiveRecoveryRecord(t *testing.T, layout instance.Layout, runID string, now time.Time) string {
	t.Helper()
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
	ref, err := recovery.RefForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	record := recovery.Record{
		Version: 1, RunID: runID, RepositoryKey: repo.CanonicalKey(), Ref: ref,
		BaseSHA: strings.Repeat("a", 40), SnapshotSHA: strings.Repeat("b", 40),
		PatchDigest:   "sha256:" + strings.Repeat("c", 64),
		ArchiveDigest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("fixture"))),
		ArchiveBytes:  7, CreatedAt: now.Add(-48 * time.Hour), RetainUntil: now.Add(24 * time.Hour),
	}
	key := sha256.Sum256([]byte(record.RepositoryKey + "\x00" + record.RunID + "\x00" + record.SnapshotSHA))
	directory := filepath.Join(layout.Root, "recovery", fmt.Sprintf("%x", key))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, recovery.BundleFileName), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recovery.PublishRecord(filepath.Join(directory, recovery.RecordFileName), record); err != nil {
		t.Fatal(err)
	}
	return directory
}

// TestTelemetryPruneRefusesJournalWithLiveRecoverySnapshot is #4824's AC #5:
// a run that still owns a live (non-retired) recovery record must never have
// its journal deleted by telemetry retention — every recovery consumer
// (retirement, selection, restore) re-opens the owning run journal and fails
// once it is gone, permanently stranding the record's inventory slot. Once
// the record is retired, the journal becomes prunable again.
func TestTelemetryPruneRefusesJournalWithLiveRecoverySnapshot(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 0)
	now := time.Now().UTC()
	runID := "owns-live-snapshot"
	runDir := createTelemetryRetentionRun(t, layout, runID, now.Add(-48*time.Hour))
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}

	directory := seedLiveRecoveryRecord(t, layout, runID, now)

	config := instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}
	if _, _, err := pruneTelemetryRetention(layout, config, db, now, false); err == nil {
		t.Fatal("prune succeeded despite the run owning a live recovery snapshot")
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("journal deleted despite a live recovery snapshot: %v", err)
	}

	// Retire the record (same ".retired-<digest>" convention RetireSnapshot
	// itself produces) — the guard must stop blocking this run once its
	// only recovery record is no longer live.
	retired := filepath.Join(filepath.Dir(directory), ".retired-"+filepath.Base(directory))
	if err := os.Rename(directory, retired); err != nil {
		t.Fatal(err)
	}
	results, _, err := pruneTelemetryRetention(layout, config, db, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RunID != runID {
		t.Fatalf("prune after retirement = %#v, want the run pruned", results)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("journal survived after the recovery snapshot was retired: %v", err)
	}
}

func prepareHeldTelemetryRetentionPass(
	t *testing.T,
) (instance.Layout, *rollup.DB, *journal.InstanceLog, string) {
	t.Helper()
	layout := writeRecoveryPolicyInstance(t, 0)
	now := time.Now().UTC()
	runID := "prepared-then-held"
	runDir := createTelemetryRetentionRun(t, layout, runID, now.Add(-48*time.Hour))
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	// Freeze a prepared pass the way a crash between prepare and delete does.
	writes := 0
	crashAfterPrepare := func(layout instance.Layout, state telemetryRetentionState) error {
		writes++
		if err := writeTelemetryRetentionState(layout, state); err != nil {
			return err
		}
		if writes == 1 {
			return errors.New("simulated crash")
		}
		return nil
	}
	config := instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500, FirstEnable: "immediate"}
	if _, _, err := pruneAndRecordTelemetryRetentionWithWriter(log, layout, config, db, now, crashAfterPrepare); err == nil {
		t.Fatal("pre-crash pass unexpectedly succeeded")
	}
	state, ok, err := readTelemetryRetentionState(layout)
	if err != nil || !ok || state.PendingTelemetryPass == nil ||
		state.PendingTelemetryPass.Phase != telemetryRetentionPassPrepared {
		t.Fatalf("frozen prepared pass: ok=%v state=%+v err=%v", ok, state, err)
	}

	// The owning snapshot appears before the daemon restarts, so the pass
	// that was prepared against a clear inventory now meets a live owner.
	seedLiveRecoveryRecord(t, layout, runID, now)
	return layout, db, log, runDir
}

// TestTelemetryRetentionReconcileSurvivesHeldRecoveryCustody is #5505. The
// custody guard also runs while COMPLETING an interrupted pass during
// startup, and a refusal there used to abort the daemon. Because it returned
// before the pass advanced past its prepared phase, the pending manifest
// stayed on disk, so every subsequent startup replayed the same refusal and
// exited non-zero — a restart loop no operator command could break, since
// the only thing that retires a snapshot is the sweep that runs after the
// readiness the daemon never reached. A refusal must now preserve the
// journal, retire the pending pass, and let startup continue.
func TestTelemetryRetentionReconcileSurvivesHeldRecoveryCustody(t *testing.T) {
	layout, db, log, runDir := prepareHeldTelemetryRetentionPass(t)

	_, reconciled, err := reconcilePendingTelemetryRetentionPass(log, layout, db, writeTelemetryRetentionState)
	if !errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatalf("reconcile error = %v, want it to wrap retention.ErrCustodyHeld", err)
	}
	if !reconciled {
		t.Fatal("reconcile reported no pending pass to complete")
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("held custody deleted the journal it exists to protect: %v", err)
	}

	// The loop fix: the pending pass must not survive in its prepared phase.
	state, ok, err := readTelemetryRetentionState(layout)
	if err != nil {
		t.Fatal(err)
	}
	if ok && state.PendingTelemetryPass != nil &&
		state.PendingTelemetryPass.Phase == telemetryRetentionPassPrepared {
		t.Fatalf("pending pass still prepared after a held custody: %+v", state.PendingTelemetryPass)
	}

	// The next startup is a no-op rather than a replay of the same refusal.
	if _, _, err := reconcilePendingTelemetryRetentionPass(log, layout, db, writeTelemetryRetentionState); err != nil {
		t.Fatalf("second reconcile = %v, want the prepared pass already retired", err)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("second reconcile deleted the protected journal: %v", err)
	}
}

func TestTelemetryRetentionReconcileDoesNotSuppressCustodyAcknowledgementFailure(t *testing.T) {
	layout, db, log, runDir := prepareHeldTelemetryRetentionPass(t)
	writes := 0
	ackErr := errors.New("injected acknowledgement failure")
	writeWithAckFailure := func(layout instance.Layout, state telemetryRetentionState) error {
		writes++
		if writes == 2 {
			return ackErr
		}
		return writeTelemetryRetentionState(layout, state)
	}

	_, reconciled, err := reconcilePendingTelemetryRetentionPass(log, layout, db, writeWithAckFailure)
	if !reconciled {
		t.Fatal("reconcile reported no pending pass to complete")
	}
	if !errors.Is(err, ackErr) {
		t.Fatalf("reconcile error = %v, want acknowledgement failure", err)
	}
	if errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatalf("reconcile error = %v, custody marker would hide acknowledgement failure", err)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("held custody deleted the journal it exists to protect: %v", err)
	}

	// The completed pass remains retryable: the next startup de-duplicates the
	// journal event and retries only the failed acknowledgement.
	if _, _, err := reconcilePendingTelemetryRetentionPass(log, layout, db, writeTelemetryRetentionState); err != nil {
		t.Fatalf("acknowledgement retry: %v", err)
	}
	state, ok, err := readTelemetryRetentionState(layout)
	if err != nil {
		t.Fatal(err)
	}
	if ok && state.PendingTelemetryPass != nil {
		t.Fatalf("acknowledged pass remains pending: %+v", state.PendingTelemetryPass)
	}
}

// TestTelemetryPruneIgnoresRecoveryRecordsForOtherRuns proves the guard is
// scoped per-run: a live recovery record belonging to a DIFFERENT run must
// not block deletion of a candidate run that owns no record of its own.
func TestTelemetryPruneIgnoresRecoveryRecordsForOtherRuns(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 0)
	now := time.Now().UTC()
	candidateRunID := "no-recovery-record"
	runDir := createTelemetryRetentionRun(t, layout, candidateRunID, now.Add(-48*time.Hour))
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}

	seedLiveRecoveryRecord(t, layout, "owns-live-snapshot", now)

	config := instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}
	results, _, err := pruneTelemetryRetention(layout, config, db, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RunID != candidateRunID {
		t.Fatalf("prune results = %#v, want the unrelated candidate run pruned", results)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("candidate journal survived: %v", err)
	}
}
