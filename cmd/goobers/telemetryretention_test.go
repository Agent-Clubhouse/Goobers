package main

import (
	"bytes"
	"os"
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
	if err := db.IngestRun(runDir); err != nil {
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
	if err := db.IngestRun(runDir); err != nil {
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
