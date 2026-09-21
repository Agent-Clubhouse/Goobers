package main

import (
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

// A single future deadline must not mask an uncovered sibling. Only a complete
// set of live stage executions establishes the earliest actual deadline.
func fleetExecutionDeadline(runs []readservice.RunSummary, gaggle string, now time.Time) time.Time {
	var earliest time.Time
	activeRuns := 0
	for _, run := range runs {
		if run.Gaggle != gaggle {
			return time.Time{}
		}
		if run.Terminal {
			continue
		}
		activeRuns++
		if run.ActivityTruncated || run.WaitingForGate || len(run.ActiveStages) == 0 {
			return time.Time{}
		}
		for _, stage := range run.ActiveStages {
			if stage.ExecutionObservedAt == nil || stage.ExecutionObservedAt.IsZero() || stage.ExecutionObservedAt.After(now) || stage.Kind != "stage" || stage.ExecutionOverlap || stage.ExecutionID == "" || stage.ExecutionDeadline == nil || stage.ExecutionDeadline.IsZero() {
				return time.Time{}
			}
			if earliest.IsZero() || stage.ExecutionDeadline.Before(earliest) {
				earliest = *stage.ExecutionDeadline
			}
		}
	}
	if activeRuns == 0 {
		return time.Time{}
	}
	return earliest
}
