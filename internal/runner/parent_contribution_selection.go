package runner

import (
	"errors"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// repoFrom selects the latest declared producer that executed. A parallel list
// covers alternative producers; it does not merge their independent trees.
// While a branch runs, sibling publications are never candidates for its input.
func selectParentContribution(events []journal.Event, task apiv1.Task, branch int) (journal.Event, bool, error) {
	boundary := parentParallelBoundary(events, branch)
	if branch > 0 && boundary == 0 {
		return journal.Event{}, false, errors.New("parent branch lacks durable parallel occurrence")
	}
	var own, upstream journal.Event
	var ownOrder, upstreamOrder uint64
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ParentContributionKind {
			continue
		}
		if _, err := decodeParentContribution(event); err != nil {
			return event, false, err
		}
		order := parentContributionFinishedSequence(events, event)
		if order == 0 {
			continue
		}
		if branch > 0 && event.Seq > boundary {
			if event.Branch == branch && order > ownOrder {
				own, ownOrder = event, order
			}
			continue
		}
		if order > upstreamOrder {
			upstream, upstreamOrder = event, order
		}
	}
	fork := branch > 0 && own.Seq == 0
	selected := upstream
	if own.Seq != 0 {
		selected = own
	}
	if selected.Seq == 0 {
		if len(task.RepoFrom) != 0 {
			return selected, fork, errors.New("parent repoFrom predecessor has no durable contribution")
		}
		return selected, fork, nil
	}
	if selected.Stage != task.Name && !slices.Contains(task.RepoFrom, selected.Stage) {
		return selected, fork, errors.New("parent latest executed producer is not declared in repoFrom")
	}
	return selected, fork, nil
}

func parentParallelBoundary(events []journal.Event, branch int) uint64 {
	if branch == 0 {
		return 0
	}
	var boundary uint64
	for _, event := range events {
		if event.Type == journal.EventParallelStarted {
			boundary = event.Seq
		}
	}
	return boundary
}

func parentContributionFinishedSequence(events []journal.Event, receipt journal.Event) uint64 {
	for _, event := range events {
		if event.Seq <= receipt.Seq || event.Branch != receipt.Branch || event.Stage != receipt.Stage {
			continue
		}
		if event.Type == journal.EventStageStarted {
			return 0
		}
		if event.Type == journal.EventStageFinished && event.Attempt == receipt.Attempt {
			return event.Seq
		}
	}
	return 0
}
