package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestRecoveryWindowUsesDurableFinishNotRetryTime(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	finish := start.Add(45 * 24 * time.Hour)
	events := []journal.Event{{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated), Time: finish}}
	for range 2 {
		got, err := recoveryWindowTime(events, start)
		if err != nil || !got.Equal(finish) {
			t.Fatalf("terminal capture time = %s, error %v", got, err)
		}
		// A cleanup observation is not a new terminal transition.
		events = append(events, journal.Event{Type: journal.EventRunnerAnnotation, Time: finish.Add(time.Hour)})
	}
	events = append(events, journal.Event{Type: journal.EventRunResumed, Time: finish.Add(2 * time.Hour)})
	if got, err := recoveryWindowTime(events, start); err != nil || !got.Equal(start) {
		t.Fatalf("resumed run reused a previous finish: %s %v", got, err)
	}
}

func TestRecoveryWindowRefusesInvalidFinishTime(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, stamp := range []time.Time{{}, start.Add(-time.Second)} {
		got, err := recoveryWindowTime([]journal.Event{{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed), Time: stamp}}, start)
		if err == nil || !got.IsZero() {
			t.Fatalf("invalid finish acknowledged: %s %v", got, err)
		}
	}
}
