package runner

import (
	"encoding/json"
	"errors"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/worktree"
)

// Host-only fork receipts bind source reservation and acquired workspace custody.
const (
	ParentForkPlannedKind    = "isolated.parent.fork.planned"
	ParentForkReadyKind      = "isolated.parent.fork.ready"
	maxParentForkPlanBytes   = 128 << 10
	maxParentForkBundleBytes = 16 << 20
)

// ParentForkPlan pins one source and the entire physical fan-out before creation.
// This is host custody, not a workflow input or a worker-produced artifact.
type ParentForkPlan struct {
	Version    int                     `json:"version"`
	Parallel   string                  `json:"parallel"`
	Sequence   uint64                  `json:"sequence"`
	Gaggle     string                  `json:"gaggle"`
	Source     spec.Source             `json:"source"`
	RunID      string                  `json:"runId"`
	Workspaces []worktree.StageCustody `json:"workspaces"`
}

type parentForkState struct {
	plan      ParentForkPlan
	reference journal.Ref
	plannedAt uint64
	ready     map[int]bool
}

func readParentForkStates(reader *journal.Reader, events []journal.Event) (map[uint64]*parentForkState, error) {
	states := map[uint64]*parentForkState{}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		switch event.Runner["kind"] {
		case ParentForkPlannedKind:
			state, err := readParentForkPlan(reader, event)
			if err != nil {
				return nil, err
			}
			if states[state.plan.Sequence] != nil {
				return nil, errors.New("duplicate parallel fork plan")
			}
			states[state.plan.Sequence] = state
		case ParentForkReadyKind:
			if err := consumeParentForkReady(states, event); err != nil {
				return nil, err
			}
		}
	}
	return states, nil
}

func readParentForkPlan(reader *journal.Reader, event journal.Event) (*parentForkState, error) {
	var ref journal.Ref
	data, err := json.Marshal(event.Runner["plan"])
	if err != nil || json.Unmarshal(data, &ref) != nil || ref.Integrity != apiv1.IntegrityTrusted {
		return nil, errors.New("invalid parallel fork plan reference")
	}
	data, err = reader.ArtifactBytesBounded(ref, maxParentForkPlanBytes)
	if err != nil {
		return nil, err
	}
	var plan ParentForkPlan
	if json.Unmarshal(data, &plan) != nil || plan.Version != 1 || plan.Sequence == 0 || plan.Sequence >= event.Seq || plan.Parallel == "" || event.Parallel != plan.Parallel || event.Branch != 0 || len(plan.Workspaces) == 0 || len(plan.Workspaces) > 128 {
		return nil, errors.New("invalid parallel fork plan")
	}
	id, err := reader.Identity()
	if err != nil {
		return nil, err
	}
	if plan.RunID != id.RunID || plan.Gaggle != id.Gaggle || id.Child != nil {
		return nil, errors.New("parallel fork plan owner changed")
	}
	if !blobstore.ValidDigest(plan.Source.Metadata.Digest) || plan.Source.Metadata.Size <= 0 || plan.Source.Metadata.Size > maxParentForkPlanBytes || !blobstore.ValidDigest(plan.Source.Bundle.Digest) || plan.Source.Bundle.Size <= 0 || plan.Source.Bundle.Size > maxParentForkBundleBytes {
		return nil, errors.New("parallel fork source identity changed")
	}

	return &parentForkState{plan: plan, reference: ref, plannedAt: event.Seq, ready: map[int]bool{}}, nil
}

func consumeParentForkReady(states map[uint64]*parentForkState, event journal.Event) error {
	value, err := decodeParentForkCustody(event)
	if err != nil {
		return err
	}
	state := states[value.Sequence]
	if state == nil || event.Branch <= 0 || event.Branch > len(state.plan.Workspaces) || event.Parallel != state.plan.Parallel || event.Seq <= state.plannedAt || value.PlannedAt != state.plannedAt || state.ready[event.Branch] || !reflect.DeepEqual(state.reference, value.Plan) || value.Workspace != state.plan.Workspaces[event.Branch-1] {
		return errors.New("parallel fork readiness differs from durable plan")
	}
	state.ready[event.Branch] = true
	return nil
}

// Reserve all declared forks together under the root execution owner. Existing
// contained-parent holds and earlier visits share the same 128-checkout bound.
func reserveParentForks(events []journal.Event, states map[uint64]*parentForkState, proposed []worktree.StageCustody) error {
	owners := map[string]worktree.StageCustody{}
	add := func(owner worktree.StageCustody) error {
		if previous, exists := owners[owner.WorkspaceID]; exists && previous != owner {
			return errors.New("parallel fork workspace ownership changed")
		}
		if owner.WorkspaceID == "" || owner.Branch == "" {
			return errors.New("invalid parent workspace ownership")
		}
		owners[owner.WorkspaceID] = owner
		if len(owners) > 128 {
			return errors.New("parent workspace fork limit exceeded before fan-out")
		}
		return nil
	}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ContainedParentWorkspaceKind {
			continue
		}
		var custody ContainedParentWorkspaceCustody
		data, err := json.Marshal(event.Runner["custody"])
		if err != nil || len(data) > 8192 || json.Unmarshal(data, &custody) != nil || custody.Version != 1 || custody.Origin == nil {
			return errors.New("invalid parent custody before fork reservation")
		}
		if err := add(custody.Workspace); err != nil {
			return err
		}
	}
	for _, state := range states {
		for _, owner := range state.plan.Workspaces {
			if err := add(owner); err != nil {
				return err
			}
		}
	}
	for _, owner := range proposed {
		if err := add(owner); err != nil {
			return err
		}
	}
	return nil
}

// Every reserved fork must have an acknowledged physical owner. Incomplete
// creation retains the plan and source until recovery can settle that custody.
func requireParallelForkArchiveOwnership(reader *journal.Reader, events []journal.Event) error {
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return err
	}
	for _, fork := range states {
		for branch, owner := range fork.plan.Workspaces {
			if !fork.ready[branch+1] {
				return ErrParentReturnPending
			}
			state, err := readParentWorkspaceState(events, owner.OwnerRunID, owner.Branch)
			if err != nil {
				return err
			}
			if state.heldAt == 0 || state.hold.Workspace != owner {
				return ErrParentReturnPending
			}
		}
	}
	return nil
}
