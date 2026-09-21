package main

import (
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

// A gaggle is wholly in retry backoff only when every nonterminal run has
// current timer evidence and no other active stage/gate can make progress.
// The earliest deadline ends that assertion; later timers cannot mask it.
func fleetRetryBackoff(runs []readservice.RunSummary, gaggle string, now, boot time.Time) time.Time {
	var earliest time.Time
	for _, run := range runs {
		if run.Gaggle != gaggle {
			return time.Time{}
		}
		if run.Terminal {
			continue
		}
		if run.WaitingForGate || run.ActivityTruncated || len(run.ActiveStages) > 0 || run.RetryBackoff.Truncated || len(run.RetryBackoff.Waits) == 0 {
			return time.Time{}
		}
		for _, wait := range run.RetryBackoff.Waits {
			if wait.ObservedAt.After(now) || !wait.Deadline.After(now) || wait.Driver == "local" && wait.ObservedAt.Before(boot) {
				return time.Time{}
			}
			if earliest.IsZero() || wait.Deadline.Before(earliest) {
				earliest = wait.Deadline
			}
		}
	}
	return earliest
}
