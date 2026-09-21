package history

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func validRecord(now time.Time) telemetry.DiagnosticRecord {
	return telemetry.DiagnosticRecord{Time: now, Name: "goobers.fleet.heartbeat", Attributes: map[string]any{"schemaVersion": 1, "deploymentId": "d", "instanceId": "i", "component": "daemon", "bootId": "b", "bootStartedAt": now.Format(time.RFC3339Nano), "sequence": 1, "observedAt": now.Format(time.RFC3339Nano), "windowStart": now.Format(time.RFC3339Nano), "windowCoverage": "complete", "state": "idle", "reasonCode": "no_eligible_work", "diagnosticsDroppedRecords": 3}}
}
func openStore(t *testing.T, dir string, scrubber journal.Scrubber) *Store {
	t.Helper()
	store, err := Open(context.Background(), dir, scrubber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
func TestSnapshotBoundsRestartAndCrashScratch(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir, nil)
	now := time.Now().UTC()
	record := validRecord(now)
	for _, key := range []string{"organization", "environment", "ownerRef", "deploymentId", "instanceId", "bootId", "platform", "version", "buildCommit", "channel"} {
		record.Attributes[key] = strings.Repeat("x", 256)
	}
	batch := make([]telemetry.DiagnosticRecord, MaxBatch)
	for i := range batch {
		batch[i] = record
	}
	for range 18 {
		if err := store.Append(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) > MaxRecords || snapshot.EvictedRecords == 0 {
		t.Fatal("unbounded/unreported retention", len(snapshot.Records), snapshot.Metadata)
	}
	info, err := os.Stat(filepath.Join(dir, snapshotName))
	if err != nil || info.Size() > MaxBytes {
		t.Fatal("snapshot exceeded bytes", info, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("snapshot not private", info.Mode())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, scratchName), []byte("crash residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := openStore(t, dir, nil)
	if _, err := os.Stat(filepath.Join(dir, scratchName)); !os.IsNotExist(err) {
		t.Fatal("fixed crash scratch retained", err)
	}
	if err := restarted.Append(context.Background(), []telemetry.DiagnosticRecord{validRecord(now)}); err != nil {
		t.Fatal(err)
	}
	after, err := Read(dir)
	if err != nil || after.EvictedRecords < snapshot.EvictedRecords {
		t.Fatal(after.Metadata, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatal("unexpected generation files", entries)
	}
}
func TestFailureRecoveryCancellationAndContention(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir, nil)
	record := validRecord(time.Now().UTC())
	if err := store.Append(context.Background(), []telemetry.DiagnosticRecord{record}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir, nil); err == nil {
		t.Fatal("second writer acquired lock")
	}
	before, err := os.ReadFile(filepath.Join(dir, snapshotName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, scratchName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), []telemetry.DiagnosticRecord{record}); err == nil {
		t.Fatal("blocked atomic scratch accepted")
	}
	after, err := os.ReadFile(filepath.Join(dir, snapshotName))
	if err != nil || string(after) != string(before) {
		t.Fatal("failed commit damaged prior snapshot", err)
	}
	if err := os.Remove(filepath.Join(dir, scratchName)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Append(ctx, []telemetry.DiagnosticRecord{record}); err == nil {
		t.Fatal("canceled append accepted")
	}
	if err := store.Append(context.Background(), []telemetry.DiagnosticRecord{record}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Read(dir)
	if err != nil || snapshot.KnownWriteFailures != 2 || len(snapshot.Records) != 2 {
		t.Fatal(snapshot.Metadata, len(snapshot.Records), err)
	}
}
func TestCorruptAndOversizedSnapshotResetBoundedly(t *testing.T) {
	for name, content := range map[string]string{"corrupt": "broken", "oversized": strings.Repeat("x", MaxBytes+1)} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, snapshotName), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(dir); err == nil {
				t.Fatal("corrupt/oversized history accepted")
			}
			store := openStore(t, dir, nil)
			if err := store.Append(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			snapshot, err := Read(dir)
			if err != nil || !snapshot.Reset || len(snapshot.Records) != 0 {
				t.Fatal(snapshot, err)
			}
		})
	}
}
func TestSnapshotScrubbingAndClosedRecordContract(t *testing.T) {
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte("private-secret-marker"))
	dir := t.TempDir()
	store := openStore(t, dir, journal.Chain(registry, journal.NewPatternScrubber()))
	record := validRecord(time.Now().UTC())
	record.Attributes["ownerRef"] = "private-secret-marker"
	bad := validRecord(record.Time)
	bad.Attributes["prompt"] = "private-prompt-marker"
	if err := store.Append(context.Background(), []telemetry.DiagnosticRecord{record, bad}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, snapshotName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-secret-marker") || strings.Contains(string(data), "private-prompt-marker") {
		t.Fatal("private data retained")
	}
	snapshot, err := Read(dir)
	if err != nil || snapshot.OmittedRecords != 1 || len(snapshot.Records) != 1 {
		t.Fatal(snapshot.Metadata, err)
	}
	if err := store.Append(context.Background(), make([]telemetry.DiagnosticRecord, MaxBatch+1)); err == nil {
		t.Fatal("oversized input batch accepted")
	}
}
func TestSnapshotRejectsExcessRecordCountBeforePopulation(t *testing.T) {
	dir := t.TempDir()
	record := validRecord(time.Now().UTC())
	raw, err := encodeRecord(record, nil)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]json.RawMessage, MaxRecords+1)
	for i := range records {
		records[i] = raw
	}
	data, err := encodeSnapshot(Metadata{StoredAt: time.Now()}, records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("oversized population accepted")
	}
}
func BenchmarkBoundedHistoryPulse(b *testing.B) {
	store, err := Open(context.Background(), b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	batch := make([]telemetry.DiagnosticRecord, 197)
	for i := range batch {
		batch[i] = validRecord(time.Now().UTC())
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := store.Append(context.Background(), batch); err != nil {
			b.Fatal(err)
		}
	}
}

// TestHistoryPulseMeasurement reports actual file sync and replacement costs in
// the normal hosted unit suite. Measurements are evidence, not timing gates.
func TestHistoryPulseMeasurement(t *testing.T) {
	for _, count := range []int{1, 197} {
		name := "idle"
		if count > 1 {
			name = "100-gaggle-pulse"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store := openStore(t, dir, nil)
			batch := make([]telemetry.DiagnosticRecord, count)
			now := time.Now().UTC()
			for i := range batch {
				batch[i] = validRecord(now)
			}
			// Fill the retained population before measuring so a loaded pulse includes
			// rewriting the retained snapshot, not only the first empty-store write.
			for filled := 0; filled < MaxRecords; filled += MaxBatch {
				seed := make([]telemetry.DiagnosticRecord, MaxBatch)
				for i := range seed {
					seed[i] = validRecord(now)
				}
				if err := store.Append(context.Background(), seed); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now()
			if err := store.Append(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(started)
			info, err := os.Stat(filepath.Join(dir, snapshotName))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Records) != MaxRecords || snapshot.EvictedRecords != uint64(count) {
				t.Fatal("measurement did not exercise a full retained window", snapshot.Metadata, len(snapshot.Records))
			}
			t.Logf("bounded history: pulse_records=%d retained_records=%d wall=%s snapshot_bytes=%d file_syncs=1 directory_syncs=1", count, len(snapshot.Records), elapsed, info.Size())
		})
	}
}

func TestSnapshotRejectsUnknownOuterFieldsWithoutPreservingPrivateRawData(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	record := validRecord(now)
	raw, err := json.Marshal(map[string]any{"name": record.Name, "time": now, "attributes": record.Attributes, "prompt": "private-seeded-prompt-marker"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := encodeSnapshot(Metadata{StoredAt: now}, []json.RawMessage{raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("unknown outer record field accepted")
	}
	store := openStore(t, dir, nil)
	if err := store.Append(context.Background(), []telemetry.DiagnosticRecord{record}); err != nil {
		t.Fatal(err)
	}
	rewritten, err := os.ReadFile(filepath.Join(dir, snapshotName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), "private-seeded-prompt-marker") || strings.Contains(string(rewritten), "prompt") {
		t.Fatal("private seeded field survived rewrite")
	}
	snapshot, err := Read(dir)
	if err != nil || !snapshot.Reset || len(snapshot.Records) != 1 {
		t.Fatal(snapshot.Metadata, len(snapshot.Records), err)
	}
}
