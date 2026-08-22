package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

type recordingHeartbeatJournal struct {
	mu     sync.Mutex
	events []journal.Event
}

func (j *recordingHeartbeatJournal) Append(event journal.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, event)
	return nil
}

func (j *recordingHeartbeatJournal) ObserveActivity() {}

func (j *recordingHeartbeatJournal) RepairAppendBoundary() error { return nil }

func (j *recordingHeartbeatJournal) heartbeatCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	count := 0
	for _, event := range j.events {
		if event.Type == journal.EventStageHeartbeat {
			count++
		}
	}
	return count
}

func TestStageHeartbeatSurvivesContextCancellation(t *testing.T) {
	ticker := &fakeHeartbeatTicker{
		ticks:   make(chan time.Time),
		stopped: make(chan struct{}),
	}
	runner := &Runner{
		heartbeatInterval:  StageHeartbeatInterval,
		newHeartbeatTicker: func(time.Duration) heartbeatTicker { return ticker },
	}
	jr := &recordingHeartbeatJournal{}

	ctx, cancel := context.WithCancel(context.Background())
	stageCtx, heartbeat := runner.startStageHeartbeat(ctx, jr, "local-ci", 1, journal.AttemptPolicy)
	cancel()

	for expected := 1; expected <= 3; expected++ {
		invoke.ReportProgress(stageCtx)
		select {
		case ticker.ticks <- time.Now():
		case <-time.After(5 * time.Second):
			t.Fatalf("heartbeat tick %d was not consumed after context cancellation", expected)
		}
		deadline := time.Now().Add(2 * time.Second)
		for jr.heartbeatCount() < expected && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := jr.heartbeatCount(); got < expected {
			t.Fatalf("heartbeat count = %d, want at least %d after cancellation", got, expected)
		}
	}

	if err := heartbeat.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStageHeartbeatStopsWhenStageEnds(t *testing.T) {
	ticker := &fakeHeartbeatTicker{
		ticks:   make(chan time.Time),
		stopped: make(chan struct{}),
	}
	runner := &Runner{
		heartbeatInterval:  StageHeartbeatInterval,
		newHeartbeatTicker: func(time.Duration) heartbeatTicker { return ticker },
	}

	_, heartbeat := runner.startStageHeartbeat(context.Background(), &recordingHeartbeatJournal{}, "local-ci", 1, journal.AttemptPolicy)
	if err := heartbeat.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("ticker was not stopped when the stage ended")
	}
}
