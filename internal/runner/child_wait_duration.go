package runner

import (
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
	if len(suspended) == 0 {
		return journal.ChildExecutionElapsed(events, startedAt, now, nil)
	}
	intervals, err := journal.ChildWaitIntervals(events, startedAt, now, nil)
	if err != nil {
		return 0, err
	}
	windows := append([]hostsuspend.Window(nil), suspended...)
	for _, interval := range intervals {
		windows = append(windows, hostsuspend.Window{From: interval.From, To: interval.To})
	}
	return now.Sub(startedAt) - hostsuspend.Overlap(windows, startedAt, now), nil
}
