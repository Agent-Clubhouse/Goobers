package scheduler

import (
	"context"
	"testing"
)

func TestServeDispatchesUntilClosed(t *testing.T) {
	st := &fakeStarter{}
	s := newScheduler(t, Config{Starter: st})

	events := make(chan Event, 2)
	events <- backlogEvent()
	events <- backlogEvent() // same item → second is a no-op (already running)
	close(events)

	var decisions []Decision
	err := s.Serve(context.Background(), events, func(_ Event, d Decision, err error) {
		if err != nil {
			t.Errorf("unexpected dispatch error: %v", err)
		}
		decisions = append(decisions, d)
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(decisions))
	}
	if !decisions[0].Started {
		t.Error("first event should start a run")
	}
	if decisions[1].Started {
		t.Error("second (duplicate) event should not start a second run")
	}
}

func TestServeStopsOnContextCancel(t *testing.T) {
	s := newScheduler(t, Config{Starter: &fakeStarter{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Serve(ctx, make(chan Event), nil); err == nil {
		t.Error("expected context cancellation to end Serve with an error")
	}
}
