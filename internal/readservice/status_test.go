package readservice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestParseProviderQuotaResumeTime(t *testing.T) {
	resetAt := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	reason := localscheduler.ReasonProviderQuota + ": resumes at " + resetAt.Format(time.RFC3339)
	got, ok := parseProviderQuotaResumeTime(reason)
	if !ok || !got.Equal(resetAt) {
		t.Fatalf("parseProviderQuotaResumeTime(%q) = %v, %v; want %v, true", reason, got, ok, resetAt)
	}

	for _, invalid := range []string{
		localscheduler.ReasonMaxParallel,
		localscheduler.ReasonBudget,
		"provider-quota",
		localscheduler.ReasonProviderQuota + ": resumes at not-a-time",
	} {
		if _, ok := parseProviderQuotaResumeTime(invalid); ok {
			t.Errorf("parseProviderQuotaResumeTime(%q) = ok, want rejected", invalid)
		}
	}
}

func TestSchedulerStatusProjectsLatestProviderQuotaPause(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	staleReset := time.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC)
	activeReset := staleReset.Add(time.Hour)
	for _, event := range []journal.Event{
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonProviderQuota + ": resumes at " + staleReset.Format(time.RFC3339)},
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonMaxParallel},
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonProviderQuota + ": resumes at " + activeReset.Format(time.RFC3339)},
	} {
		if err := log.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.ProviderQuotaResumeAt == nil || !status.ProviderQuotaResumeAt.Equal(activeReset) {
		t.Fatalf("ProviderQuotaResumeAt = %v, want %v", status.ProviderQuotaResumeAt, activeReset)
	}
}

func TestSchedulerStatusProjectsLatestDaemonRestartAndRecoveredRuns(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	startedAt := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	eventTime := startedAt.Add(-time.Hour)
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time {
		return eventTime
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventDaemonStarted}); err != nil {
		t.Fatal(err)
	}
	eventTime = startedAt.Add(-time.Second)
	if err := log.Append(journal.Event{
		Type:   journal.EventDaemonDirtyRestart,
		Reason: "process exited unexpectedly",
	}); err != nil {
		t.Fatal(err)
	}
	eventTime = startedAt
	if err := log.Append(journal.Event{
		Type: journal.EventDaemonStarted,
		Runner: map[string]any{
			"pid":          42,
			"version":      "v1.2.3",
			"instanceRoot": "/srv/goobers",
		},
	}); err != nil {
		t.Fatal(err)
	}
	eventTime = startedAt.Add(time.Second)
	if err := log.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: "run-resumed",
		Runner: map[string]any{
			"kind":   journal.RunnerAnnotationRunRecovery,
			"action": journal.RecoveryActionResumed,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: "run-new",
		Runner: map[string]any{
			"kind":   journal.RunnerAnnotationTriggerRecovery,
			"action": journal.RecoveryActionNewClaim,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := status.DaemonRestart
	if got == nil ||
		!got.At.Equal(startedAt) ||
		got.Reason != "process exited unexpectedly" ||
		got.PID != 42 ||
		got.Version != "v1.2.3" ||
		got.Root != "/srv/goobers" {
		t.Fatalf("DaemonRestart = %+v", got)
	}
	if len(got.RunIDs) != 1 || got.RunIDs[0] != "run-resumed" {
		t.Fatalf("recovered run IDs = %v, want [run-resumed]", got.RunIDs)
	}
}

func TestListStatusRunsSkipsMalformedHistoricalRuns(t *testing.T) {
	service, layout, machine := fixtureService(t)
	startedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	healthy, _ := createFixtureRun(
		t,
		layout,
		machine,
		"healthy-run",
		"implementation",
		"goobers",
		startedAt,
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)
	if err := healthy.Close(); err != nil {
		t.Fatal(err)
	}
	malformed, _ := createFixtureRun(
		t,
		layout,
		machine,
		"malformed-run",
		"implementation",
		"goobers",
		startedAt.Add(-time.Minute),
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)
	if err := malformed.Append(journal.Event{Type: journal.EventRunFinished, Status: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := malformed.Close(); err != nil {
		t.Fatal(err)
	}

	runs, err := service.ListStatusRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "healthy-run" {
		t.Fatalf("ListStatusRuns = %+v, want only healthy-run", runs)
	}
}

func writeInitCompleted(t *testing.T, layout instance.Layout, at time.Time) {
	t.Helper()
	instanceLog, _, err := journal.OpenInstanceLog(
		layout.SchedulerDir(),
		journal.WithClock(func() time.Time { return at }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Append(journal.Event{Type: journal.EventInitCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTimeToFirstPRUsesInitCompletionAndOpenedRef(t *testing.T) {
	service, layout, machine := fixtureService(t)
	initCompletedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	writeInitCompleted(t, layout, initCompletedAt)
	first, firstClock := createFixtureRun(
		t, layout, machine, "first-run", "implementation", "goobers",
		initCompletedAt.Add(3*time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	firstClock.now = initCompletedAt.Add(4 * time.Minute)
	if err := first.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: "42"},
		Runner:      map[string]any{"operation": "claim"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, secondClock := createFixtureRun(
		t, layout, machine, "second-run", "implementation", "goobers",
		initCompletedAt.Add(5*time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	secondClock.now = initCompletedAt.Add(8 * time.Minute)
	if err := second.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "7"},
		Runner:      map[string]any{"operation": "update"},
	}); err != nil {
		t.Fatal(err)
	}
	secondClock.now = initCompletedAt.Add(12 * time.Minute)
	if err := second.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "8"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	metric, err := service.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) {
		t.Fatalf("InitCompletedAt = %v, want %v", metric.InitCompletedAt, initCompletedAt)
	}
	wantOpenAt := initCompletedAt.Add(12 * time.Minute)
	if metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(wantOpenAt) {
		t.Fatalf("FirstPROpenAt = %v, want %v", metric.FirstPROpenAt, wantOpenAt)
	}
	if metric.Milliseconds == nil || *metric.Milliseconds != (12*time.Minute).Milliseconds() {
		t.Fatalf("Milliseconds = %v, want %d", metric.Milliseconds, (12 * time.Minute).Milliseconds())
	}
}

func TestTimeToFirstPRIgnoresPersistedPROpenBeforeLegacyInitCompletion(t *testing.T) {
	_, layout, machine := fixtureService(t)
	initCompletedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	legacy, legacyClock := createFixtureRun(
		t, layout, machine, "legacy-run", "implementation", "goobers",
		initCompletedAt.Add(-time.Hour), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	legacyClock.now = initCompletedAt.Add(-30 * time.Minute)
	if err := legacy.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "7"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	legacyDir := legacy.Dir()
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), legacyDir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	writeInitCompleted(t, layout, initCompletedAt)
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	metric, err := service.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) ||
		metric.FirstPROpenAt != nil || metric.Milliseconds != nil {
		t.Fatalf("TimeToFirstPR after legacy no-op init = %#v", metric)
	}

	current, currentClock := createFixtureRun(
		t, layout, machine, "current-run", "implementation", "goobers",
		initCompletedAt.Add(time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	currentClock.now = initCompletedAt.Add(5 * time.Minute)
	if err := current.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "8"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}

	metric, err = service.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(currentClock.now) ||
		metric.Milliseconds == nil || *metric.Milliseconds != (5*time.Minute).Milliseconds() {
		t.Fatalf("TimeToFirstPR after post-init PR = %#v", metric)
	}
}

func TestTimeToFirstPRFailsClosedOnUnreadableJournal(t *testing.T) {
	service, layout, machine := fixtureService(t)
	startedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	healthy, clock := createFixtureRun(
		t, layout, machine, "healthy-run", "implementation", "goobers",
		startedAt, journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock.now = startedAt.Add(12 * time.Minute)
	if err := healthy.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "8"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := healthy.Close(); err != nil {
		t.Fatal(err)
	}

	unreadable, _ := createFixtureRun(
		t, layout, machine, "unreadable-run", "implementation", "goobers",
		startedAt.Add(-time.Hour), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	if err := unreadable.Close(); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(layout.RunsDir(), "unreadable-run", "events.jsonl")
	if err := os.WriteFile(eventsPath, []byte("{]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if metric, err := service.TimeToFirstPR(context.Background()); err == nil {
		t.Fatalf("TimeToFirstPR = %#v, nil; want unreadable journal error", metric)
	}
}

func TestTimeToFirstPRFailsClosedOnUnreadableInstanceJournal(t *testing.T) {
	service, layout, _ := fixtureService(t)
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.SchedulerDir(), "events.jsonl"), []byte("{]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if metric, err := service.TimeToFirstPR(context.Background()); err == nil {
		t.Fatalf("TimeToFirstPR = %#v, nil; want unreadable instance journal error", metric)
	}
}

func TestTimeToFirstPRUsesMilestoneAfterJournalRetention(t *testing.T) {
	_, layout, machine := fixtureService(t)
	initCompletedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	writeInitCompleted(t, layout, initCompletedAt)
	first, _ := createFixtureRun(
		t, layout, machine, "first-run", "implementation", "goobers",
		initCompletedAt.Add(2*time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	firstDir := first.Dir()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, clock := createFixtureRun(
		t, layout, machine, "first-pr-run", "implementation", "goobers",
		initCompletedAt.Add(5*time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock.now = initCompletedAt.Add(12 * time.Minute)
	if err := second.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "8"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	secondDir := second.Dir()
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestSchedulerLog(context.Background(), layout.SchedulerDir()); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}
	for _, dir := range []string{firstDir, secondDir} {
		if err := db.IngestRun(context.Background(), dir); err != nil {
			t.Fatalf("IngestRun(%s): %v", dir, err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	metric, err := service.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) ||
		metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(clock.now) ||
		metric.Milliseconds == nil || *metric.Milliseconds != (12*time.Minute).Milliseconds() {
		t.Fatalf("TimeToFirstPR after retention = %#v", metric)
	}
}

func TestSchedulerStatusPropagatesReadAndContextFailures(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.SchedulerDir(), "events.jsonl"), []byte("{]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SchedulerStatus(context.Background()); err == nil {
		t.Fatal("SchedulerStatus succeeded with a malformed instance journal")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.SchedulerStatus(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("SchedulerStatus canceled error = %v, want context.Canceled", err)
	}
}
