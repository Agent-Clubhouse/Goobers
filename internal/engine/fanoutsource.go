package engine

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

func (r *runJournal) fanOutSource(
	m *wf.Machine,
	source apiv1.FanOutSource,
) (journal.FanOutSourceRef, int64, error) {
	task, ok := m.Task(source.Task)
	if !ok {
		return journal.FanOutSourceRef{}, 0, fmt.Errorf("engine: fan-out producer %q is not a task", source.Task)
	}
	slotIndex := -1
	limit := fanout.MaxSourceBytes
	for i, slot := range task.ArtifactSlots {
		if slot.Name != source.Slot {
			continue
		}
		slotIndex = i
		if slot.MaxSize > 0 && slot.MaxSize < limit {
			limit = slot.MaxSize
		}
		break
	}
	if slotIndex < 0 {
		return journal.FanOutSourceRef{}, 0, fmt.Errorf(
			"engine: fan-out source slot %s.%s is not declared",
			source.Task, source.Slot,
		)
	}
	projection := r.projectionSnapshot()
	for i := len(projection.Ops) - 1; i >= 0; i-- {
		finished := projection.Ops[i].Event
		if finished == nil || finished.Branch != 0 || finished.Type != journal.EventStageFinished ||
			finished.Stage != source.Task {
			continue
		}
		if finished.Status != string(apiv1.ResultSuccess) {
			return journal.FanOutSourceRef{}, 0, fmt.Errorf(
				"engine: fan-out producer %q attempt %d finished %q, not success",
				source.Task, finished.Attempt, finished.Status,
			)
		}
		if slotIndex >= len(finished.Artifacts) {
			return journal.FanOutSourceRef{}, 0, fmt.Errorf(
				"engine: fan-out source slot %s.%s has no journaled artifact",
				source.Task, source.Slot,
			)
		}
		for j := i - 1; j >= 0; j-- {
			started := projection.Ops[j].Event
			if started == nil || started.Branch != 0 || started.Type != journal.EventStageStarted ||
				started.Stage != source.Task || started.Attempt != finished.Attempt {
				continue
			}
			visit, _ := started.Runner["artifactVisit"].(uint64)
			if visit == 0 {
				return journal.FanOutSourceRef{}, 0, fmt.Errorf(
					"engine: fan-out producer %q has no artifact publication visit",
					source.Task,
				)
			}
			ref := finished.Artifacts[slotIndex]
			return journal.FanOutSourceRef{
				Task: source.Task, Slot: source.Slot, Visit: visit, Attempt: finished.Attempt,
				Digest: ref.Digest, Integrity: ref.Integrity, Path: ref.Path,
				Size: ref.Size, MediaType: ref.MediaType,
			}, limit, nil
		}
		return journal.FanOutSourceRef{}, 0, fmt.Errorf(
			"engine: fan-out producer %q attempt %d has no matching start",
			source.Task, finished.Attempt,
		)
	}
	return journal.FanOutSourceRef{}, 0, fmt.Errorf(
		"engine: fan-out producer %q has no settled root attempt",
		source.Task,
	)
}
