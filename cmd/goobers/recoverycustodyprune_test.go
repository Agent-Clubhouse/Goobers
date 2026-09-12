package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

// TestTelemetryPruneRefusesJournalWithLiveRecoverySnapshot is #4824's AC #5:
// a run that still owns a live (non-retired) recovery record must never have
// its journal deleted by telemetry retention — every recovery consumer
// (retirement, selection, restore) re-opens the owning run journal and fails
// once it is gone, permanently stranding the record's inventory slot. Once
// the record is retired, the journal becomes prunable again.
func TestTelemetryPruneRefusesJournalWithLiveRecoverySnapshot(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
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

// TestTelemetryPruneIgnoresRecoveryRecordsForOtherRuns proves the guard is
// scoped per-run: a live recovery record belonging to a DIFFERENT run must
// not block deletion of a candidate run that owns no record of its own.
func TestTelemetryPruneIgnoresRecoveryRecordsForOtherRuns(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
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

	otherRunID := "owns-live-snapshot"
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
	ref, err := recovery.RefForRun(otherRunID)
	if err != nil {
		t.Fatal(err)
	}
	record := recovery.Record{
		Version: 1, RunID: otherRunID, RepositoryKey: repo.CanonicalKey(), Ref: ref,
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
