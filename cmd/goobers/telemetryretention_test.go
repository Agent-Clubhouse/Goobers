package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
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

func TestTelemetryRetentionStartupSummaryIsBounded(t *testing.T) {
	results := make([]retention.Result, 25)
	var stdout bytes.Buffer
	reportTelemetryPruned(&stdout, results, true, true)
	if got, want := stdout.String(), "telemetry retention: candidates=25 mode=dry-run\n"; got != want {
		t.Fatalf("startup summary = %q, want %q", got, want)
	}
}

func TestRecordTelemetryRetentionPassJournalsBoundedProjection(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	enforceAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := writeTelemetryRetentionState(layout, telemetryRetentionState{EnforceAt: enforceAt}); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	results := make([]retention.Result, 3)
	if err := recordTelemetryRetentionPass(log, layout, instance.TelemetryRetentionConfig{}, results, true); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != journal.EventTelemetryRetentionPass ||
		events[0].Runner["mode"] != "dry-run" || events[0].Runner["candidateCount"] != float64(3) ||
		events[0].Runner["enforceAt"] != enforceAt.Format(time.RFC3339Nano) {
		t.Fatalf("retention pass event = %+v", events)
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
	// Two runs dated well past every check point this test advances through
	// (including the final post-grace one), so the one stale candidate is a
	// minority of this instance's history (#4824's large-first-enforcement
	// gate only blocks when a pass would prune the majority) — this test
	// exercises the timed grace window specifically, not that gate.
	for _, id := range []string{"recent-1", "recent-2"} {
		recentDir := createTelemetryRetentionRun(t, runLayout, id, now.Add(30*24*time.Hour))
		if err := db.IngestRun(context.Background(), recentDir); err != nil {
			t.Fatal(err)
		}
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
	afterGrace := now.Add(retentionGraceWindow + time.Hour)
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

func TestConfiguredTelemetryRetentionPersistsConservativeClockRepair(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	state := telemetryRetentionState{
		DetectedAt: now.Add(time.Hour),
		EnforceAt:  now.Add(time.Hour).Add(retentionGraceWindow),
		TotalRuns:  9,
	}
	if err := writeTelemetryRetentionState(layout, state); err != nil {
		t.Fatal(err)
	}
	if _, dryRun, err := pruneConfiguredTelemetryRetention(layout, instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}, db, now); err != nil {
		t.Fatal(err)
	} else if !dryRun {
		t.Fatal("future-dated grace state allowed telemetry deletion")
	}

	got, ok, err := readTelemetryRetentionState(layout)
	if err != nil || !ok {
		t.Fatalf("read repaired state: ok=%v err=%v", ok, err)
	}
	if !got.DetectedAt.Equal(now) || !got.EnforceAt.Equal(now.Add(retentionGraceWindow)) {
		t.Fatalf("persisted repair = (%s, %s), want fresh window from %s", got.DetectedAt, got.EnforceAt, now)
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
	runLayout := instanceLayout.ForGaggle("example")
	runDir := createTelemetryRetentionRun(t, runLayout, "first-real-candidate", later.Add(-48*time.Hour))
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}
	// Two runs dated well past this test's final post-grace check point, so
	// the one stale candidate stays a minority of history — #4824's
	// large-first-enforcement gate is a separate test.
	for _, id := range []string{"recent-1", "recent-2"} {
		recentDir := createTelemetryRetentionRun(t, runLayout, id, later.Add(60*24*time.Hour))
		if err := db.IngestRun(context.Background(), recentDir); err != nil {
			t.Fatal(err)
		}
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

// TestConfiguredTelemetryRetentionLargeFirstEnforcementRequiresAcknowledgement
// is #4824's reported scenario: DefaultTelemetryRetentionMaxRuns binds long
// before the documented window at production run rates, so a first
// enforcement pass can queue the vast majority of an instance's history for
// deletion. Even after the timed grace window elapses, such a pass must stay
// dry until the operator explicitly acknowledges it (firstEnable: immediate)
// — the timed window alone authorized a *report*, not a majority wipe nobody
// reviewed.
func TestConfiguredTelemetryRetentionLargeFirstEnforcementRequiresAcknowledgement(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := initDeterministicDemo(t)
	instanceLayout := instance.NewLayout(root)
	runLayout := instanceLayout.ForGaggle("example")
	db, err := rollup.Open(instanceLayout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// 9 stale runs, 1 recent one: enforcing right now would prune 90% of
	// this instance's history.
	staleDirs := make([]string, 9)
	for i := range 9 {
		runDir := createTelemetryRetentionRun(t, runLayout, fmt.Sprintf("stale-%d", i), now.Add(-48*time.Hour))
		if err := db.IngestRun(context.Background(), runDir); err != nil {
			t.Fatal(err)
		}
		staleDirs[i] = runDir
	}
	// Dated to stay within the window at every check point this test
	// advances through, including the final post-acknowledgement one.
	recentDir := createTelemetryRetentionRun(t, runLayout, "recent", now.Add(30*24*time.Hour))
	if err := db.IngestRun(context.Background(), recentDir); err != nil {
		t.Fatal(err)
	}

	config := instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}

	// First pass: the timed grace window starts, same as always.
	if _, dryRun, err := pruneConfiguredTelemetryRetention(instanceLayout, config, db, now); err != nil {
		t.Fatal(err)
	} else if !dryRun {
		t.Fatal("first pass over pre-existing excess data must be a dry run (grace window)")
	}

	// After the grace window elapses, an ordinary pass would enforce for
	// real — but pruning 9 of 10 runs exceeds the large-first-enforcement
	// fraction, so this must stay dry instead.
	afterGrace := now.Add(retentionGraceWindow + time.Hour)
	results, dryRun, err := pruneConfiguredTelemetryRetention(instanceLayout, config, db, afterGrace)
	if err != nil {
		t.Fatal(err)
	}
	if !dryRun {
		t.Fatal("a first enforcement pruning 90% of history must stay dry-run without explicit acknowledgement")
	}
	if len(results) != 9 {
		t.Fatalf("blocked pass results = %#v, want all 9 stale candidates reported", results)
	}
	for i, dir := range staleDirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("blocked pass deleted journal stale-%d: %v", i, err)
		}
	}
	state, ok, err := readTelemetryRetentionState(instanceLayout)
	if err != nil || !ok {
		t.Fatalf("readTelemetryRetentionState: ok=%v err=%v", ok, err)
	}
	if !state.LargeFirstEnforceBlocked {
		t.Fatalf("state = %+v, want LargeFirstEnforceBlocked", state)
	}
	if state.TotalRuns != 10 {
		t.Fatalf("state.TotalRuns = %d, want 10", state.TotalRuns)
	}

	// The operator reviews and explicitly acknowledges — immediate now
	// proceeds despite the same lopsided ratio.
	config.FirstEnable = "immediate"
	results, dryRun, err = pruneConfiguredTelemetryRetention(instanceLayout, config, db, afterGrace.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if dryRun {
		t.Fatal("an explicit firstEnable: immediate acknowledgement must proceed even for a majority-of-history prune")
	}
	if len(results) != 9 {
		t.Fatalf("acknowledged prune results = %#v, want all 9 stale runs pruned", results)
	}
	for i, dir := range staleDirs {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("acknowledged enforcement left journal stale-%d: %v", i, err)
		}
	}
	if _, err := os.Stat(recentDir); err != nil {
		t.Fatalf("acknowledged enforcement deleted the recent run: %v", err)
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
		DetectedAt:       now.Add(-time.Hour),
		EnforceAt:        now.Add(-time.Hour).Add(retentionGraceWindow),
		LastPassAt:       now.Add(-time.Minute),
		LastPassDryRun:   true,
		CandidateCount:   3,
		TotalRuns:        20,
		OldestRetainedAt: now.Add(-72 * time.Hour),
	}
	if err := writeTelemetryRetentionState(layout, graceState); err != nil {
		t.Fatal(err)
	}
	var duringGrace bytes.Buffer
	reportTelemetryRetentionPolicy(layout, now, &duringGrace)
	if !strings.Contains(duringGrace.String(), "grace period active") ||
		!strings.Contains(duringGrace.String(), "3 of 20 run") ||
		!strings.Contains(duringGrace.String(), "history retained back to 72h0m0s ago") ||
		strings.Contains(duringGrace.String(), "pruned") {
		t.Fatalf("grace-period status = %q", duringGrace.String())
	}

	// #4824: enforcement held pending explicit operator acknowledgement.
	blockedState := telemetryRetentionState{
		DetectedAt:               now.Add(-retentionGraceWindow - time.Hour),
		EnforceAt:                now.Add(-time.Hour),
		LastPassAt:               now.Add(-time.Minute),
		LastPassDryRun:           true,
		LargeFirstEnforceBlocked: true,
		CandidateCount:           18,
		TotalRuns:                20,
	}
	if err := writeTelemetryRetentionState(layout, blockedState); err != nil {
		t.Fatal(err)
	}
	var blocked bytes.Buffer
	reportTelemetryRetentionPolicy(layout, now, &blocked)
	if !strings.Contains(blocked.String(), "held for explicit acknowledgement") ||
		!strings.Contains(blocked.String(), "18 of 20 run") ||
		!strings.Contains(blocked.String(), "firstEnable: immediate") ||
		!strings.Contains(blocked.String(), "no run would survive") {
		t.Fatalf("acknowledgement-blocked status = %q", blocked.String())
	}

	enforcedState := telemetryRetentionState{
		LastPassAt:       now.Add(-time.Minute),
		LastPassDryRun:   false,
		PrunedCount:      7,
		TotalRuns:        13,
		OldestRetainedAt: now.Add(-24 * time.Hour),
	}
	if err := writeTelemetryRetentionState(layout, enforcedState); err != nil {
		t.Fatal(err)
	}
	var enforced bytes.Buffer
	reportTelemetryRetentionPolicy(layout, now, &enforced)
	if !strings.Contains(enforced.String(), "policy in force") ||
		!strings.Contains(enforced.String(), "pruned 7 run") ||
		!strings.Contains(enforced.String(), "history retained back to 24h0m0s ago") {
		t.Fatalf("enforced status = %q", enforced.String())
	}
}
