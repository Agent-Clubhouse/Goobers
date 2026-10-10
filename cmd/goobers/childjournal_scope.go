package main

import (
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/journal"
)

// Harness lifecycle/progress records carry their scope inside the typed payload.
// Verify that scope against the signed contract before filling absent outer
// fields. Explicit foreign outer fields still fail the ordinary projection.
func childObservationScope(c childpod.Contract, event journal.Event) (journal.Event, error) {
	check := func(run, stage string, attempt int) bool {
		return run == c.Identity.RunID && stageArtifactName(run, stage) == c.Stage && attempt == c.Attempt
	}
	scoped := false
	if event.Agent != nil {
		if !check(event.Agent.RunID, event.Agent.Stage, event.Agent.Attempt) {
			return event, childJournalRefusal()
		}
		scoped = true
	}
	if event.Progress != nil {
		if !check(event.Progress.RunID, event.Progress.Stage, event.Progress.Attempt) {
			return event, childJournalRefusal()
		}
		scoped = true
	}
	if scoped && event.Stage == "" {
		event.Stage = c.Stage
	}
	return event, nil
}
