package childworkflow

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

func activeJournalStage(ctx context.Context, rd *journal.Reader, runID string, origin apiv1.ChildWorkflowOrigin) (journal.RunIdentity, journal.Event, error) {
	if err := ctx.Err(); err != nil {
		return journal.RunIdentity{}, journal.Event{}, err
	}
	id, err := rd.Identity()
	if err != nil || !pinnedRunIdentity(id, runID) {
		return id, journal.Event{}, errors.Join(ErrAuthorityUnavailable, err)
	}
	events, err := rd.Events()
	if err != nil || journal.PhaseFromEvents(events) != journal.PhaseRunning {
		return id, journal.Event{}, errors.Join(ErrAuthorityUnavailable, err)
	}
	index := boundStageIndex(events, runID, origin)
	if index < 0 {
		return id, journal.Event{}, ErrAuthorityUnavailable
	}
	start := events[index]
	parallel := stageParallel(events[:index], start.Branch)
	for _, event := range events[index+1:] {
		if supersedesStage(start, event) || parallel != "" && event.Type == journal.EventParallelFinished && event.Parallel == parallel {
			return id, start, ErrAuthorityUnavailable
		}
	}
	return id, start, nil
}

func stageParallel(events []journal.Event, branch int) string {
	if branch == 0 {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventBranchStarted && events[i].Branch == branch {
			return events[i].Parallel
		}
	}
	return ""
}

func pinnedRunIdentity(id journal.RunIdentity, runID string) bool {
	return id.KnownSchema() && id.RunID == runID && submissionText(id.Gaggle, 128) &&
		submissionText(id.Workflow, 256) && id.WorkflowVersion > 0 &&
		blobstore.ValidDigest(id.ConfigGeneration) && blobstore.ValidDigest(id.WorkflowDigest) &&
		blobstore.ValidDigest(id.GooberDigest) && (id.Driver == "" || id.Driver == journal.DriverEngine)
}

// An occurrence must trace to an original committed start on this exact branch
// and task. A guessed occurrence, or an occurrence borrowed from a sibling, is
// insufficient even when its attempt hash happens to match a later record.
func boundStageIndex(events []journal.Event, runID string, wanted apiv1.ChildWorkflowOrigin) int {
	roots := map[string]journal.Event{}
	var previous uint64
	index := -1
	for i, event := range events {
		if event.Seq <= previous {
			return -1
		}
		previous = event.Seq
		if event.Type != journal.EventStageStarted {
			continue
		}
		origin, err := journal.ChildWorkflowOriginForEvent(runID, event)
		if err != nil || event.Attempt < 1 {
			continue
		}
		if origin.StageOccurrence == origin.AttemptID {
			roots[origin.StageOccurrence] = event
		}
		root, found := roots[origin.StageOccurrence]
		if *origin == wanted && found && root.Stage == event.Stage && root.Branch == event.Branch {
			index = i
		}
	}
	return index
}

func supersedesStage(start, next journal.Event) bool {
	switch next.Type {
	case journal.EventRunStarted, journal.EventRunResumed, journal.EventRunFinished:
		return true
	}
	if next.Branch != start.Branch {
		return false
	}
	switch next.Type {
	case journal.EventStageStarted, journal.EventBranchFinished, journal.EventGateStarted,
		journal.EventGatePaused, journal.EventGateOverridden, journal.EventParallelStarted, journal.EventParallelFinished:
		return true
	case journal.EventStageFinished:
		return next.Stage == start.Stage && (next.Attempt == 0 || next.Attempt == start.Attempt)
	case journal.EventStageRerunRequested:
		return next.Stage == start.Stage
	}
	return false
}
