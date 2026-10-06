package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sqliteuri"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// This name is included by the existing native Windows JournalCatchup gate.
// It exercises actual configured retention and export with synthetic local
// journals, not an elapsed seven-day daemon or a capacity/performance claim.
func TestJournalCatchupRetentionBoundary(t *testing.T) {
	// TestMain disables journal fsync for ordinary command fixtures. This
	// durability boundary deliberately restores it for these small cases.
	t.Setenv("GOOBERS_DISABLE_FSYNC", "0")
	if journal.FsyncDisabled() {
		t.Fatal("retention replay boundary requires real journal fsync")
	}
	for _, tc := range []struct {
		name               string
		mature, obstructed bool
	}{
		{"grace preserves unspooled source", false, true},
		{"mature source pruning preserves durable replay", true, false},
		{"mature unspooled source cannot be reconstructed", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDeterministicDemo(t)
			layout := instance.NewLayout(root)
			spool := filepath.Join(root, "telemetry-export", "azure-monitor")
			receiver, server := newRetentionReplayReceiver(t)
			cfg := telemetry.Config{
				AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
				AzureMonitorHTTPClient:       server.Client(), AzureMonitorJournalLogs: true,
				JournalLogs: true, JournalLogsOnly: true, JournalRoot: root, JournalInstanceID: "retention-fixture",
				AzureMonitorReplayRoot: spool, AzureMonitorReplayMaxAge: 72 * time.Hour, AzureMonitorReplayMaxBytes: 1 << 20,
			}
			// Persist enrollment before creating any run. This separates source
			// deletion from the intentionally excluded pre-enrollment history.
			primer := startRetentionReplayClient(t, cfg)
			waitRetentionReplay(t, func() bool { return telemetry.InspectAzureReplayRoot(spool).AccountingReady })
			flushRetentionReplayClient(t, primer)
			stopRetentionReplayClient(t, primer)
			verifyRetentionEnrollment(t, spool)
			var restore func()
			if tc.obstructed {
				restore = obstructRetentionReplay(t, spool)
			}
			client := startRetentionReplayClient(t, cfg)
			db, err := rollup.Open(layout.TelemetryDB())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			dirs, expected := createRetentionReplayRuns(t, layout, db)
			waitRetentionReplay(t, func() bool {
				if tc.obstructed {
					return client.JournalExportStats().ExportFailures > 0
				}
				return telemetry.InspectAzureReplayRoot(spool).PendingRecords >= len(expected)
			})
			stopRetentionReplayClient(t, client)
			applyRetentionReplayPolicy(t, layout, db, tc.mature)
			wanted := retainedReplayKeys(t, dirs, expected, tc.mature, tc.obstructed)
			if restore != nil {
				restore()
			}
			receiver.allow.Store(true)
			restarted := startRetentionReplayClient(t, cfg)
			waitRetentionReplay(t, func() bool { return receiver.hasKeys(wanted) })
			stopRetentionReplayClient(t, restarted)
			receiver.verify(t, wanted)
			t.Logf("source events=%d recovered=%d mature=%v admission-obstructed=%v; source-policy loss=%d", len(expected), len(wanted), tc.mature, tc.obstructed, len(expected)-len(wanted))
		})
	}
}

func verifyRetentionEnrollment(t *testing.T, spool string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(spool, ".journal-cursors.db"))+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var since int64
	if err := db.QueryRowContext(ctx, "SELECT since FROM enrollment WHERE id=1").Scan(&since); err != nil || since <= 0 || since > time.Now().UnixNano() {
		t.Fatalf("enrollment was not persisted before source creation: since=%d err=%v", since, err)
	}
}

func flushRetentionReplayClient(t *testing.T, client *telemetry.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("flush replay fixture: %v", err)
	}
}

func startRetentionReplayClient(t *testing.T, cfg telemetry.Config) *telemetry.Client {
	t.Helper()
	client, err := telemetry.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopRetentionReplayClient(t, client) })
	return client
}

func stopRetentionReplayClient(t *testing.T, client *telemetry.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		t.Errorf("shutdown replay fixture: %v", err)
	}
}

func waitRetentionReplay(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("retention/replay fixture did not settle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func createRetentionReplayRuns(t *testing.T, layout instance.Layout, db *rollup.DB) (map[string]string, map[string]string) {
	t.Helper()
	dirs, keys := map[string]string{}, map[string]string{}
	started := time.Now().UTC()
	for i := range 6 {
		id := fmt.Sprintf("retention-replay-%d", i)
		dir := createTelemetryRetentionRun(t, layout.ForGaggle("example"), id, started.Add(time.Duration(i)*time.Millisecond))
		dirs[id] = dir
		if err := db.IngestRun(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
		reader, err := journal.OpenRead(dir)
		if err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			keys[fmt.Sprintf("%s:%d", id, event.Seq)] = id
		}
	}
	return dirs, keys
}

func applyRetentionReplayPolicy(t *testing.T, layout instance.Layout, db *rollup.DB, mature bool) {
	t.Helper()
	now := time.Now().UTC()
	state := telemetryRetentionState{}
	if mature {
		state.DetectedAt = now.Add(-8 * 24 * time.Hour)
		state.EnforceAt = state.DetectedAt.Add(retentionGraceWindow)
		state.EnforceAcknowledged = true
	}
	if err := writeTelemetryRetentionState(layout, state); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	count, dry, err := pruneAndRecordTelemetryRetention(log, layout, instance.TelemetryRetentionConfig{Window: "90d", MaxRuns: 3}, db, now)
	if err != nil || count != 3 || dry == mature {
		t.Fatalf("retention pass: count=%d dry=%v err=%v", count, dry, err)
	}
	state, ok, err := readTelemetryRetentionState(layout)
	if err != nil || !ok || state.PendingTelemetryPass != nil || state.LastPassDryRun == mature {
		t.Fatalf("retention summary was not durably acknowledged: %+v %v", state, err)
	}
	pruned := 0
	if mature {
		pruned = 3
	}
	if state.PrunedCount != pruned {
		t.Fatalf("pruned count=%d want=%d", state.PrunedCount, pruned)
	}
}

func retainedReplayKeys(t *testing.T, dirs, expected map[string]string, mature, obstructed bool) map[string]string {
	t.Helper()
	retained := map[string]bool{}
	for id, dir := range dirs {
		_, err := os.Stat(dir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		retained[id] = err == nil
	}
	count := 0
	for _, exists := range retained {
		if exists {
			count++
		}
	}
	wantCount := 6
	if mature {
		wantCount = 3
	}
	if count != wantCount {
		t.Fatalf("remaining source runs=%d want=%d", count, wantCount)
	}
	wanted := map[string]string{}
	for key, id := range expected {
		if !obstructed || retained[id] {
			wanted[key] = id
		}
	}
	if mature && obstructed && len(wanted) >= len(expected) {
		t.Fatal("fixture did not exercise source-policy loss")
	}
	return wanted
}
