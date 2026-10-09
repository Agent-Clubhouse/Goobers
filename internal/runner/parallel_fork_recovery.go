package runner

import (
	"errors"
	"sort"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// ParentForkRecovery identifies unacknowledged physical checkouts in a durable
// fan-out plan. The host verifies and imports its source before acquiring them.
type ParentForkRecovery struct {
	Plan      ParentForkPlan
	Reference journal.Ref
	Pending   []int
}

// PendingParentForks includes plans interrupted before any branch launched.
// Its bounded reservation remains authoritative until every checkout is held.
func PendingParentForks(reader *journal.Reader) ([]ParentForkRecovery, error) {
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return nil, err
	}
	if err := reserveParentForks(events, states, nil); err != nil {
		return nil, err
	}
	var result []ParentForkRecovery
	for _, state := range states {
		pending := ParentForkRecovery{Plan: state.plan, Reference: state.reference}
		for index := range state.plan.Workspaces {
			if !state.ready[index+1] {
				pending.Pending = append(pending.Pending, index+1)
			}
		}
		if len(pending.Pending) != 0 {
			result = append(result, pending)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Plan.Sequence < result[j].Plan.Sequence })
	return result, nil
}

// RecordParentForkReady acknowledges exact acquired host custody under the root
// journal lease. It neither provisions a checkout nor invents an agent return.
// Identical recovery retries are a no-op; changed plans/owners are refused.
func RecordParentForkReady(run *journal.Run, sequence uint64, reference journal.Ref, branch int, owner worktree.StageCustody) error {
	if run == nil {
		return errors.New("parallel fork readiness needs its owned journal")
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return err
	}
	state := states[sequence]
	if state == nil || reference != state.reference || branch <= 0 || branch > len(state.plan.Workspaces) || owner != state.plan.Workspaces[branch-1] {
		return errors.New("parallel fork readiness differs from its owned reservation")
	}
	if state.ready[branch] {
		return nil
	}
	return run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Branch: branch, Parallel: state.plan.Parallel, Runner: map[string]any{"kind": ParentForkReadyKind, "sequence": sequence, "plannedAt": state.plannedAt, "plan": reference, "workspace": owner}})
}
