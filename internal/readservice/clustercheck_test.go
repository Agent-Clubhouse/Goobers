package readservice

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestClusterChecksFreshnessRecoveryAndRetention(t *testing.T) {
	service, layout, _ := fixtureService(t)
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	appendCheck := func(id, outcome string) {
		t.Helper()
		if err := log.Append(journal.Event{Type: journal.EventClusterCheckCompleted, Runner: map[string]any{
			"check": id, "outcome": outcome, "expiresAt": now.Add(time.Hour).Format(time.RFC3339Nano),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	assertStates := func(reader *Local, states ...string) {
		t.Helper()
		status, err := reader.SchedulerStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(status.ClusterChecks) != len(states) {
			t.Fatalf("checks = %+v", status.ClusterChecks)
		}
		for i, want := range states {
			if got := status.ClusterChecks[i].State; got != want {
				t.Fatalf("check %s = %s, want %s", status.ClusterChecks[i].Check, got, want)
			}
		}
	}
	assertStates(service)
	appendCheck("apiserver-ipblock-drift", "fail")
	appendCheck("overlay-pin-agreement", "pass")
	assertStates(service, "degraded", "healthy")
	// Cached projections must recompute freshness without new events.
	now = now.Add(time.Hour)
	assertStates(service, "degraded", "stale")
	status, err := service.SchedulerStatus(context.Background())
	if err != nil || !status.ClusterChecks[0].Stale {
		t.Fatalf("expired failure = %+v, %v", status, err)
	}
	// A monitor that stops while failing must not disappear after retention
	// and a reader restart.
	if _, err := log.Compact(now, now); err != nil {
		t.Fatal(err)
	}
	failedRestart, err := NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions()}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	failedRestart.now = func() time.Time { return now }
	assertStates(failedRestart, "degraded", "stale")
	// Caller mutation cannot alter the retained fold.
	status.ClusterChecks[0].Outcome = "pass"
	assertStates(service, "degraded", "stale")
	appendCheck("overlay-pin-agreement", "pass")
	assertStates(service, "degraded", "healthy")
	appendCheck("apiserver-ipblock-drift", "pass")
	assertStates(service, "healthy", "healthy")
	appendCheck("overlay-pin-agreement", "warn")
	assertStates(service, "healthy", "warning")
	if err := log.Append(journal.Event{Type: journal.EventDaemonStarted}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(48 * time.Hour)
	if _, err := log.Compact(now, now); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions()}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return now }
	assertStates(restarted, "stale", "stale")
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("retained %d events, want latest two checks", len(events))
	}
}

func TestClusterChecksConcurrentJournalWriterAndStatus(t *testing.T) {
	service, layout, _ := fixtureService(t)
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 12; i++ {
			// Separate handles match a CronJob publishing beside a live daemon.
			external, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
			if err != nil {
				t.Error(err)
				return
			}
			err = external.Append(journal.Event{Type: journal.EventClusterCheckCompleted, Runner: map[string]any{
				"check": "apiserver-ipblock-drift", "outcome": "fail", "expiresAt": time.Now().Add(time.Hour).Format(time.RFC3339Nano),
			}})
			closeErr := external.Close()
			if err != nil || closeErr != nil {
				t.Errorf("append: %v; close: %v", err, closeErr)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 12; i++ {
			if err := log.Append(journal.Event{Type: journal.EventDaemonStarted}); err != nil {
				t.Error(err)
				return
			}
			if _, err := service.SchedulerStatus(context.Background()); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	status, err := service.SchedulerStatus(context.Background())
	if err != nil || len(status.ClusterChecks) != 1 || status.ClusterChecks[0].State != "degraded" {
		t.Fatalf("status = %+v, %v", status, err)
	}
}
