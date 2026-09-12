package localscheduler

import (
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// reconstructBudgetStarts counts admissions, not their journal echoes. The
// scheduler records an admission before starting its driver; the engine starter
// also records run.started for visibility. Both identify the same budget charge.
// Keep the first event in journal order, including when it predates the cutoff:
// a later echo must not extend a rolling window or undo an explicit rate reset.
// Legacy events without run IDs remain separate charges because their identity
// cannot be established. Workflow resolution retains its conservative legacy
// scoping, and equal run ID spellings in different scopes stay independent.
func reconstructBudgetStarts(events []journal.Event, runsDirs []string, workflows []WorkflowIdentity, cutoff time.Time) map[WorkflowIdentity][]time.Time {
	type admission struct {
		workflow WorkflowIdentity
		runID    string
	}
	seen := make(map[admission]struct{})
	starts := make(map[WorkflowIdentity][]time.Time)
	for _, event := range events {
		if event.Type != journal.EventRunStarted {
			continue
		}
		for _, identity := range resolveRunStartedIdentities(runsDirs, event, workflows) {
			if event.RunID != "" {
				key := admission{workflow: identity, runID: event.RunID}
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
			}
			if event.Time.After(cutoff) {
				starts[identity] = append(starts[identity], event.Time)
			}
		}
	}
	return starts
}
