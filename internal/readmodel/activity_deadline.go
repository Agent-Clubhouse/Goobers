package readmodel

import (
	"slices"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Execution deadlines belong to the current attempt, not to the stage name
// forever. Overlapping process-only observations cannot prove a whole stage
// bound; retain that uncertainty until a new stage attempt starts.
func (s StageActivity) withExecutionDeadline(e journal.Event) StageActivity {
	for index, active := range s.Active {
		if active.Kind != "stage" || active.Name != e.Stage || active.Branch != e.Branch || active.Attempt != e.Attempt || e.Time.IsZero() || e.Time.Before(active.StartedAt) {
			continue
		}
		id, idOK := e.Runner["executionId"].(string)
		state, _ := e.Runner["executionState"].(string)
		raw, _ := e.Runner["deadline"].(string)
		deadline, err := time.Parse(time.RFC3339Nano, raw)
		s.Active = slices.Clone(s.Active)
		if !idOK || id == "" || len(id) > 64 || err != nil || deadline.IsZero() {
			s.Active[index].ExecutionDeadline = nil
			s.Active[index].ExecutionOverlap = true
			return s
		}
		switch state {
		case "active":
			if active.ExecutionID != "" && active.ExecutionID != id {
				active.ExecutionOverlap = true
			}
			observedAt := e.Time
			active.ExecutionObservedAt = &observedAt
			active.ExecutionID = id
			active.ExecutionDeadline = &deadline
			if active.ExecutionOverlap {
				active.ExecutionDeadline = nil
			}
		case "finished":
			if active.ExecutionID != id {
				return s
			}
			active.ExecutionID, active.ExecutionDeadline, active.ExecutionObservedAt = "", nil, nil
		default:
			active.ExecutionDeadline, active.ExecutionOverlap = nil, true
		}
		s.Active[index] = active
		return s
	}
	return s
}
