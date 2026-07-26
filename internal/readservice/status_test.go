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

func TestTimeToFirstPRUsesJournalIdentityAndOpenedRef(t *testing.T) {
	service, layout, machine := fixtureService(t)
	firstRunAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	first, firstClock := createFixtureRun(
		t, layout, machine, "first-run", "implementation", "goobers",
		firstRunAt, journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	firstClock.now = firstRunAt.Add(2 * time.Minute)
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
		firstRunAt.Add(5*time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	secondClock.now = firstRunAt.Add(8 * time.Minute)
	if err := second.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "7"},
		Runner:      map[string]any{"operation": "update"},
	}); err != nil {
		t.Fatal(err)
	}
	secondClock.now = firstRunAt.Add(12 * time.Minute)
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
	if metric.FirstRunAt == nil || !metric.FirstRunAt.Equal(firstRunAt) {
		t.Fatalf("FirstRunAt = %v, want %v", metric.FirstRunAt, firstRunAt)
	}
	wantOpenAt := firstRunAt.Add(12 * time.Minute)
	if metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(wantOpenAt) {
		t.Fatalf("FirstPROpenAt = %v, want %v", metric.FirstPROpenAt, wantOpenAt)
	}
	if metric.Milliseconds == nil || *metric.Milliseconds != (12*time.Minute).Milliseconds() {
		t.Fatalf("Milliseconds = %v, want %d", metric.Milliseconds, (12 * time.Minute).Milliseconds())
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
