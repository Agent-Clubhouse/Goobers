package runner

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// ParentForkSourceReleasedKind records host release of a temporary source pin.
// The durable source carrier remains in the owning journal for resumed imports.
const ParentForkSourceReleasedKind = "isolated.parent.fork.source.released"

// ParentForkRetirement binds a source plan to the current archive cycle of all
// its workspaces, including forks subsequently used by a contained parent.
type ParentForkRetirement struct {
	Plan            ParentForkPlan
	Reference       journal.Ref
	Retirements     []uint64
	ReleaseRecorded bool
}

type parentForkRelease struct {
	Plan        journal.Ref `json:"plan"`
	Retirements []uint64    `json:"retirements"`
}

// RetiredParentForks refuses release until every reserved workspace has current
// archive authority. A restored branch invalidates the previous release cycle.
func RetiredParentForks(reader *journal.Reader) ([]ParentForkRetirement, error) {
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil || len(states) == 0 {
		return nil, err
	}
	candidates, err := ParentRetirementCandidates(reader)
	if err != nil {
		return nil, err
	}
	byBranch := map[string]ParentRetirementCandidate{}
	for _, candidate := range candidates {
		if candidate.RetirementSeq == 0 {
			return nil, ErrParentReturnPending
		}
		byBranch[candidate.Workspace.Custody.Workspace.Branch] = candidate
	}
	var result []ParentForkRetirement
	for _, state := range states {
		value := ParentForkRetirement{Plan: state.plan, Reference: state.reference}
		for _, owner := range state.plan.AllWorkspaces() {
			candidate, ok := byBranch[owner.Branch]
			if !ok || candidate.Workspace.Custody.Workspace != owner {
				return nil, ErrParentReturnPending
			}
			value.Retirements = append(value.Retirements, candidate.RetirementSeq)
		}
		value.ReleaseRecorded, err = parentForkReleaseRecorded(events, value)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Plan.Sequence < result[j].Plan.Sequence })
	return result, nil
}

func parentForkReleaseRecorded(events []journal.Event, value ParentForkRetirement) (bool, error) {
	expected := parentForkRelease{Plan: value.Reference, Retirements: value.Retirements}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ParentForkSourceReleasedKind {
			continue
		}
		var release parentForkRelease
		data, err := json.Marshal(event.Runner["release"])
		if err != nil || len(data) > 8192 || json.Unmarshal(data, &release) != nil || event.Branch != 0 || len(release.Retirements) == 0 || len(release.Retirements) > 128 {
			return false, errors.New("invalid parallel source release receipt")
		}
		for _, sequence := range release.Retirements {
			if sequence == 0 || sequence >= event.Seq {
				return false, errors.New("parallel source release precedes its archives")
			}
		}
		if reflect.DeepEqual(release, expected) {
			if event.Parallel != value.Plan.Parallel {
				return false, errors.New("parallel source release scope changed")
			}
			return true, nil
		}
	}
	return false, nil
}

// RecordParentForkSourceRelease is called after exact pin deletion or verified
// transfer to retained archive custody. It grants no workspace removal authority.
func RecordParentForkSourceRelease(run *journal.Run, expected ParentForkRetirement) error {
	if run == nil {
		return errors.New("parallel source release requires owned journal")
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return err
	}
	values, err := RetiredParentForks(reader)
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.Reference != expected.Reference {
			continue
		}
		if !reflect.DeepEqual(value.Plan, expected.Plan) || !reflect.DeepEqual(value.Retirements, expected.Retirements) {
			return errors.New("parallel source release archive cycle changed")
		}
		if value.ReleaseRecorded {
			return nil
		}
		return run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Parallel: value.Plan.Parallel, Runner: map[string]any{"kind": ParentForkSourceReleasedKind, "release": parentForkRelease{Plan: value.Reference, Retirements: value.Retirements}}})
	}
	return errors.New("parallel source release plan missing")
}

// ParentForkPlanForWorkspace finds the original source even after a contained
// worker replaces the fork's host-only archive authority with its own return.
func ParentForkPlanForWorkspace(reader *journal.Reader, owner worktree.StageCustody) (ParentForkPlan, bool, error) {
	events, err := reader.Events()
	if err != nil {
		return ParentForkPlan{}, false, err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return ParentForkPlan{}, false, err
	}
	var selected ParentForkPlan
	for _, state := range states {
		for _, expected := range state.plan.AllWorkspaces() {
			if expected == owner && state.plan.Sequence > selected.Sequence {
				selected = state.plan
			}
		}
	}
	return selected, selected.Sequence != 0, nil
}
