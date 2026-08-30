package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestTelemetryPruneIsExplicitWhenAutomationDisabled(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root).ForGaggle("example")
	runDir := createTelemetryRetentionRun(t, layout, "explicit-old", now.Add(-100*24*time.Hour))
	db, err := rollup.Open(instance.NewLayout(root).TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runTelemetryPruneAt([]string{"--dry-run", root}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("dry-run code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `would prune run="explicit-old" reason=window`) {
		t.Fatalf("dry-run output = %q", stdout.String())
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("dry-run removed journal: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runTelemetryPruneAt([]string{root}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("prune code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `pruned run="explicit-old" reason=window`) {
		t.Fatalf("prune output = %q", stdout.String())
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("explicit prune left journal: %v", err)
	}
}

func TestConfiguredTelemetryRetentionDefaultsOffThenPrunes(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := initDeterministicDemo(t)
	instanceLayout := instance.NewLayout(root)
	runLayout := instanceLayout.ForGaggle("example")
	runDir := createTelemetryRetentionRun(t, runLayout, "automatic-old", now.Add(-48*time.Hour))
	db, err := rollup.Open(instanceLayout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}

	config := instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}
	results, err := pruneConfiguredTelemetryRetention(instanceLayout, config, db, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("disabled automatic prune results = %#v", results)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("disabled automatic retention removed journal: %v", err)
	}

	config.Enabled = true
	results, err = pruneConfiguredTelemetryRetention(instanceLayout, config, db, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RunID != "automatic-old" {
		t.Fatalf("enabled automatic prune results = %#v", results)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("enabled automatic retention left journal: %v", err)
	}
}

func TestCompactSchedulerRetentionBoundsLiveJournalAndRollup(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	eventTime := now.Add(-48 * time.Hour)
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time {
		return eventTime
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()
	if err := instanceLog.Append(journal.Event{Type: journal.EventTriggerFired, Gaggle: "g", Workflow: "monthly", Reason: "scheduled"}); err != nil {
		t.Fatal(err)
	}
	eventTime = now
	if err := instanceLog.Append(journal.Event{Type: journal.EventTickSkipped, Workflow: "recent"}); err != nil {
		t.Fatal(err)
	}

	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := compactSchedulerRetention(context.Background(), instance.TelemetryRetentionConfig{Window: "24h"}, db, instanceLog, nil, now); err != nil {
		t.Fatal(err)
	}

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Workflow != "monthly" || events[1].Workflow != "recent" {
		t.Fatalf("retained journal events = %#v", events)
	}
	eventTime = now.Add(time.Minute)
	if err := instanceLog.Append(journal.Event{Type: journal.EventTickSkipped, Workflow: "after"}); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestSchedulerLog(context.Background(), instanceLog.Dir()); err != nil {
		t.Fatal(err)
	}
	rolledUp, err := db.SchedulerEvents(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rolledUp) != 2 || rolledUp[0].Workflow != "recent" || rolledUp[1].Workflow != "after" {
		t.Fatalf("retained scheduler rows = %#v", rolledUp)
	}
}

func TestCompactSchedulerRetentionJournalsStaleGenerationCleanupFailure(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	dir := layout.SchedulerDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Generation 3 is current, and generation 1 was stranded by an earlier
	// cleanup failure. A non-empty directory standing in for the stale
	// generation file fails os.Remove identically on every platform.
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl.gen-000003"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl.current"), []byte("3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "events.jsonl.gen-000001", "held"), 0o755); err != nil {
		t.Fatal(err)
	}

	eventTime := now.Add(-48 * time.Hour)
	instanceLog, _, err := journal.OpenInstanceLog(dir, journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()
	if err := instanceLog.Append(journal.Event{Type: journal.EventTickSkipped, Workflow: "stale"}); err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Append(journal.Event{Type: journal.EventTriggerFired, Gaggle: "g", Workflow: "monthly", Reason: "scheduled"}); err != nil {
		t.Fatal(err)
	}
	eventTime = now
	if err := instanceLog.Append(journal.Event{Type: journal.EventTickSkipped, Workflow: "recent"}); err != nil {
		t.Fatal(err)
	}

	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	cleanupErrors := newSweepErrorReporter(instanceLog, "journal_generation_cleanup_failed")
	if err := compactSchedulerRetention(context.Background(), instance.TelemetryRetentionConfig{Window: "24h"}, db, instanceLog, cleanupErrors, now); err != nil {
		t.Fatalf("a stale-generation cleanup failure must not fail the retention sweep: %v", err)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic *journal.ErrorDetail
	for _, event := range events {
		if event.Error != nil && event.Error.Code == "journal_generation_cleanup_failed" {
			diagnostic = event.Error
		}
	}
	if diagnostic == nil {
		t.Fatalf("no cleanup diagnostic journaled by the daemon retention path, events = %#v", events)
	}
	if !strings.Contains(diagnostic.Message, "events.jsonl.gen-000001") {
		t.Fatalf("diagnostic %q does not name the generation that could not be removed", diagnostic.Message)
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl.gen-000001")); err != nil {
		t.Fatalf("blocked generation should still be on disk for a later sweep: %v", err)
	}
	// The compaction the diagnostic rode along with must still have happened.
	for _, event := range events {
		if event.Workflow == "stale" {
			t.Fatalf("compaction did not drop the aged record: %#v", events)
		}
	}
	if len(events) < 2 || events[0].Workflow != "monthly" || events[1].Workflow != "recent" {
		t.Fatalf("compaction did not preserve the expected records: %#v", events)
	}
}

func createTelemetryRetentionRun(t *testing.T, layout instance.Layout, runID string, startedAt time.Time) string {
	t.Helper()
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return startedAt }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordArtifact("transcript.jsonl", []byte("transcript\n")); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return run.Dir()
}
