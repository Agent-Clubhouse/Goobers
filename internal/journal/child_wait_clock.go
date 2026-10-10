package journal

import (
	"fmt"
	"time"
)

// ChildExecutionElapsed excludes only whole-run parked intervals when branch
// is nil. A selected branch excludes its own waits, including resumed waits.
// A running or queued sibling keeps the whole-run execution clock advancing.
func ChildExecutionElapsed(events []Event, startedAt, now time.Time, branch *int) (time.Duration, error) {
	intervals, err := ChildWaitIntervals(events, startedAt, now, branch)
	if err != nil {
		return 0, err
	}
	elapsed := now.Sub(startedAt)
	for _, interval := range intervals {
		elapsed -= interval.To.Sub(interval.From)
	}
	return elapsed, nil
}

// ChildWaitInterval is one verified closed interval of durable child waiting.
// Callers may union these intervals with their own host-suspension observations.
type ChildWaitInterval struct{ From, To time.Time }

// ChildWaitIntervals returns bounded-by-history wait intervals after validating
// the exact branch ownership and interval timestamps.
func ChildWaitIntervals(events []Event, startedAt, now time.Time, branch *int) ([]ChildWaitInterval, error) {
	clock := childWaitClock{started: startedAt, now: now}
	_, err := projectChildWaits(events, func(event Event, p *childWaitProjector) error {
		waiting := p.projection().Parked()
		if branch != nil {
			_, waiting = p.waits[*branch]
		}
		if waiting {
			if clock.waiting.IsZero() {
				clock.waiting = event.Time
			}
			if clock.waiting.IsZero() || clock.waiting.Before(startedAt) {
				return fmt.Errorf("runner: child wait has invalid clock custody")
			}
			return nil
		}
		return clock.end(event.Time)
	})
	if err != nil {
		return nil, err
	}
	if err := clock.end(now); err != nil {
		return nil, err
	}
	elapsed := now.Sub(startedAt)
	if clock.excluded < 0 || (clock.excluded > 0 && clock.excluded > elapsed) {
		return nil, fmt.Errorf("runner: child waiting exceeds run lifetime")
	}
	return clock.waits, nil
}

type childWaitClock struct {
	started, now, waiting time.Time
	excluded              time.Duration
	waits                 []ChildWaitInterval
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
	c.waits = append(c.waits, ChildWaitInterval{From: c.waiting, To: at})
	c.waiting = time.Time{}
	return nil
}
