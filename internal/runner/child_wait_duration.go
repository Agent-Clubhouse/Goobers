package runner

import (
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/hostsuspend"
	"github.com/goobers/goobers/internal/journal"
)

// RunExecutionElapsed excludes durable child waiting from the ordinary maximum
// run duration. Stage process timeouts already end when the stopped invocation
// joins. Explicit caller cancellation and deadlines are not changed here.
//
// It also excludes every suspended interval of the host (#5891): the journal's
// timestamps are wall-clock, so without this a host that slept spent the run's
// budget doing nothing. A suspension during a child wait is subtracted once.
func RunExecutionElapsed(events []journal.Event, startedAt, now time.Time, suspended ...hostsuspend.Window) (time.Duration, error) {
	if _, _, err := pendingChildWait(events); err != nil {
		return 0, err
	}
	clock := childWaitClock{started: startedAt, now: now}
	for _, event := range events {
		if event.Type == journal.EventStageFinished || event.Type == journal.EventRunFinished {
			if err := clock.end(event.Time); err != nil {
				return 0, err
			}
		}
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		switch event.Runner["kind"] {
		case ChildWaitKind:
			if clock.waiting.IsZero() {
				clock.waiting = event.Time
			}
			if clock.waiting.IsZero() || clock.waiting.Before(startedAt) {
				return 0, fmt.Errorf("runner: child wait has invalid clock custody")
			}
		case ChildContinuedKind:
			if err := clock.end(event.Time); err != nil {
				return 0, err
			}
		}
	}
	if err := clock.end(now); err != nil {
		return 0, err
	}
	elapsed := now.Sub(startedAt)
	// Ordinary runs retain their existing behavior if the host clock predates
	// StartedAt. A real child interval must fit within the recorded lifetime.
	if clock.excluded < 0 || (clock.excluded > 0 && clock.excluded > elapsed) {
		return 0, fmt.Errorf("runner: child waiting exceeds run lifetime")
	}
	return elapsed - hostsuspend.Overlap(append(clock.waits, suspended...), startedAt, now), nil
}

type childWaitClock struct {
	started, now, waiting time.Time
	excluded              time.Duration
	waits                 []hostsuspend.Window
}

func (c *childWaitClock) end(at time.Time) error {
	if c.waiting.IsZero() {
		return nil
	}
	if at.Before(c.waiting) || at.After(c.now) {
		return fmt.Errorf("runner: child continuation has invalid clock custody")
	}
	interval := at.Sub(c.waiting)
	if interval > c.now.Sub(c.started)-c.excluded {
		return fmt.Errorf("runner: child waiting exceeds run lifetime")
	}
	c.excluded += interval
	c.waits = append(c.waits, hostsuspend.Window{From: c.waiting, To: at})
	c.waiting = time.Time{}
	return nil
}
