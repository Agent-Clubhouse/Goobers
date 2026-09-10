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

// TestConfiguredTelemetryRetentionOptOutStartsGraceWindowThenEnforces is
// #4253/#3056's core acceptance test: automatic pruning is opt-out by
// default, but the FIRST pass over data that already exceeds policy is a
// dry run (the safe first-enable grace window) — nothing is deleted until
// that window elapses.
func TestConfiguredTelemetryRetentionOptOutStartsGraceWindowThenEnforces(t *testing.T) {
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

	results, dryRun, err := pruneConfiguredTelemetryRetention(instanceLayout, config, db, now)
	if err != nil {
		t.Fatal(err)
	}
	if !dryRun {
		t.Fatal("first pass over pre-existing excess data must be a dry run (grace window)")
	}
	if len(results) != 1 || results[0].RunID != "automatic-old" {
		t.Fatalf("grace-window candidate results = %#v", results)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("grace-window pass removed journal: %v", err)
	}

	// Still within the grace window a bit later: still a dry run.
	results, dryRun, err = pruneConfiguredTelemetryRetention(instanceLayout, config, db, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !dryRun || len(results) != 1 {
		t.Fatalf("mid-grace pass = (results %#v, dryRun %v), want 1 candidate still dry run", results, dryRun)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("mid-grace pass removed journal: %v", err)
	}

	// After the grace window elapses, enforcement begins for real.
	afterGrace := now.Add(telemetryRetentionGraceWindow + time.Hour)
	results, dryRun, err = pruneConfiguredTelemetryRetention(instanceLayout, config, db, afterGrace)
	if err != nil {
		t.Fatal(err)
	}
	if dryRun {
		t.Fatal("pass after the grace window elapsed must enforce for real")
	}
	if len(results) != 1 || results[0].RunID != "automatic-old" {
		t.Fatalf("post-grace prune results = %#v", results)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("post-grace enforcement left journal: %v", err)
	}

	state, ok, err := readTelemetryRetentionState(instanceLayout)
	if err != nil || !ok {
		t.Fatalf("readTelemetryRetentionState after enforcement: ok=%v err=%v", ok, err)
	}
	if state.LastPassDryRun || state.PrunedCount != 1 || !state.LastPassAt.Equal(afterGrace) {
		t.Fatalf("state after enforcement = %+v, want dryRun=false prunedCount=1 lastPassAt=%s", state, afterGrace)
	}
}

// TestConfiguredTelemetryRetentionNoCandidatesNeverStartsGraceWindow proves a
// fresh instance with nothing yet to prune never starts (or gets stuck in) a
// grace window it doesn't need — each pass stays a harmless dry run with zero
// candidates until real data eventually exceeds policy.
func TestConfiguredTelemetryRetentionNoCandidatesNeverStartsGraceWindow(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := initDeterministicDemo(t)
	instanceLayout := instance.NewLayout(root)
	db, err := rollup.Open(instanceLayout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	config := instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}
	results, dryRun, err := pruneConfiguredTelemetryRetention(instanceLayout, config, db, now)
	if err != nil {
		t.Fatal(err)
	}
	if !dryRun || len(results) != 0 {
		t.Fatalf("empty-instance pass = (results %#v, dryRun %v), want 0 candidates, still dry run", results, dryRun)
	}
	state, ok, err := readTelemetryRetentionState(instanceLayout)
	if err != nil || !ok {
		t.Fatalf("readTelemetryRetentionState: ok=%v err=%v", ok, err)
	}
	if !state.EnforceAt.IsZero() {
		t.Fatalf("state.EnforceAt = %s, want zero — no grace window should start with nothing to prune", state.EnforceAt)
	}
}

// TestConfiguredTelemetryRetentionStartsGraceWindowAfterPriorEmptyPass is a
// regression guard for #4253's fix-forward bug: an earlier version gated the
// "first encounter" dry-run fallback on whether a state file merely existed
// (!hasState) rather than on whether a grace window had ever actually
// started (state.EnforceAt.IsZero()). Since the state file is written on
// every pass — including a harmless empty one — that let a single 0-candidate
// pass permanently satisfy the "first pass" check: the very next pass to
// find real candidates, no matter how much later, enforced immediately with
// zero grace period. This chains exactly that sequence — an empty pass
// first, then a later pass that first finds real candidates — and requires
// the second pass to still be a dry run that starts the grace window,
// mirroring TestConfiguredTelemetryRetentionOptOutStartsGraceWindowThenEnforces's
// structure but with the empty pass prepended.
func TestConfiguredTelemetryRetentionStartsGraceWindowAfterPriorEmptyPass(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := initDeterministicDemo(t)
	instanceLayout := instance.NewLayout(root)
	db, err := rollup.Open(instanceLayout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	config := instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}

	// Pass 1: fresh instance, nothing old enough to prune yet. Correctly a
	// dry run with 0 candidates — this is the pass that used to (incorrectly)
	// "use up" the first-encounter check by merely writing a state file.
	results, dryRun, err := pruneConfiguredTelemetryRetention(instanceLayout, config, db, now)
	if err != nil {
		t.Fatalf("empty pass: %v", err)
	}
	if !dryRun || len(results) != 0 {
		t.Fatalf("empty pass = (results %#v, dryRun %v), want 0 candidates, dry run", results, dryRun)
	}

	// Weeks later, a run finally ages past policy — the first pass that ever
	// finds real candidates. This must still be a dry run (the grace window
	// starting now), not immediate enforcement.
	later := now.Add(30 * 24 * time.Hour)
	runDir := createTelemetryRetentionRun(t, instanceLayout.ForGaggle("example"), "first-real-candidate", later.Add(-48*time.Hour))
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}

	results, dryRun, err = pruneConfiguredTelemetryRetention(instanceLayout, config, db, later)
	if err != nil {
		t.Fatalf("first-candidates pass: %v", err)
	}
	if !dryRun {
		t.Fatal("first pass to find real candidates after a prior empty pass must still be a dry run (grace window), not immediate enforcement")
	}
	if len(results) != 1 || results[0].RunID != "first-real-candidate" {
		t.Fatalf("first-candidates pass results = %#v", results)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("first-candidates dry-run pass deleted the run journal: %v", err)
	}

	state, ok, err := readTelemetryRetentionState(instanceLayout)
	if err != nil || !ok {
		t.Fatalf("readTelemetryRetentionState: ok=%v err=%v", ok, err)
	}
	if state.EnforceAt.IsZero() {
		t.Fatal("grace window must have started (EnforceAt set) once real candidates were first found")
	}

	// After the grace window elapses, enforcement begins for real.
	afterGrace := state.EnforceAt.Add(time.Hour)
	results, dryRun, err = pruneConfiguredTelemetryRetention(instanceLayout, config, db, afterGrace)
	if err != nil {
		t.Fatalf("post-grace pass: %v", err)
	}
	if dryRun {
		t.Fatal("pass after the grace window elapsed must enforce for real")
	}
	if len(results) != 1 || results[0].RunID != "first-real-candidate" {
		t.Fatalf("post-grace prune results = %#v", results)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("post-grace enforcement left journal: %v", err)
	}
}

// TestConfiguredTelemetryRetentionExplicitlyDisabledIsNoOp proves
// telemetry.retention.enabled: false still fully disables automatic pruning
// under the new opt-out default.
func TestConfiguredTelemetryRetentionExplicitlyDisabledIsNoOp(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := initDeterministicDemo(t)
	instanceLayout := instance.NewLayout(root)
	runLayout := instanceLayout.ForGaggle("example")
	runDir := createTelemetryRetentionRun(t, runLayout, "kept", now.Add(-48*time.Hour))
	db, err := rollup.Open(instanceLayout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}

	disabled := false
	config := instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500, Enabled: &disabled}
	results, dryRun, err := pruneConfiguredTelemetryRetention(instanceLayout, config, db, now)
	if err != nil {
		t.Fatal(err)
	}
	if dryRun || len(results) != 0 {
		t.Fatalf("explicitly disabled retention results = (%#v, dryRun %v), want none", results, dryRun)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("disabled retention removed journal: %v", err)
	}
	if _, ok, err := readTelemetryRetentionState(instanceLayout); err != nil || ok {
		t.Fatalf("disabled retention must not record state: ok=%v err=%v", ok, err)
	}
}

// TestConfiguredTelemetryRetentionImmediateFirstEnableSkipsGraceWindow
// proves the operator escape hatch (telemetry.retention.firstEnable:
// immediate) enforces for real from the very first pass.
func TestConfiguredTelemetryRetentionImmediateFirstEnableSkipsGraceWindow(t *testing.T) {
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

	config := instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500, FirstEnable: "immediate"}
	results, dryRun, err := pruneConfiguredTelemetryRetention(instanceLayout, config, db, now)
	if err != nil {
		t.Fatal(err)
	}
	if dryRun {
		t.Fatal("firstEnable: immediate must skip the grace window even on the very first pass")
	}
	if len(results) != 1 || results[0].RunID != "automatic-old" {
		t.Fatalf("immediate prune results = %#v", results)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("immediate enforcement left journal: %v", err)
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

// TestReportTelemetryRetentionPolicySurfacesStatus is #4253's operator-
// visibility acceptance guard: `goobers status` must show policy-in-force,
// last-pass, and candidate-count without the caller needing to parse the
// state file itself, and must stay silent when there is nothing to report
// (retention disabled, or the daemon has never run a pass).
func TestReportTelemetryRetentionPolicySurfacesStatus(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())

	var silent bytes.Buffer
	reportTelemetryRetentionPolicy(layout, now, &silent)
	if silent.Len() != 0 {
		t.Fatalf("no state file yet: output = %q, want silence", silent.String())
	}

	graceState := telemetryRetentionState{
		DetectedAt:     now.Add(-time.Hour),
		EnforceAt:      now.Add(6 * 24 * time.Hour),
		LastPassAt:     now.Add(-time.Minute),
		LastPassDryRun: true,
		CandidateCount: 3,
	}
	if err := writeTelemetryRetentionState(layout, graceState); err != nil {
		t.Fatal(err)
	}
	var duringGrace bytes.Buffer
	reportTelemetryRetentionPolicy(layout, now, &duringGrace)
	if !strings.Contains(duringGrace.String(), "grace period active") ||
		!strings.Contains(duringGrace.String(), "3 candidate") ||
		strings.Contains(duringGrace.String(), "pruned") {
		t.Fatalf("grace-period status = %q", duringGrace.String())
	}

	enforcedState := telemetryRetentionState{
		LastPassAt:     now.Add(-time.Minute),
		LastPassDryRun: false,
		PrunedCount:    7,
	}
	if err := writeTelemetryRetentionState(layout, enforcedState); err != nil {
		t.Fatal(err)
	}
	var enforced bytes.Buffer
	reportTelemetryRetentionPolicy(layout, now, &enforced)
	if !strings.Contains(enforced.String(), "policy in force") ||
		!strings.Contains(enforced.String(), "pruned 7 run") {
		t.Fatalf("enforced status = %q", enforced.String())
	}
}
