package workspaceauthority

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
	MaxParentForkPlanBytes   = 128 << 10
	MaxParentForkBundleBytes = 16 << 20
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
	Root       *worktree.StageCustody  `json:"root,omitempty"`
}

// ParentForkState tracks one pinned fanout plan and its ready workspace receipts.
type ParentForkState struct {
	Plan      ParentForkPlan
	Reference journal.Ref
	PlannedAt uint64
	Ready     map[int]bool
}

// ReadParentForkStates reconstructs bounded fork plans and validates their readiness receipts.
func ReadParentForkStates(reader *journal.Reader, events []journal.Event) (map[uint64]*ParentForkState, error) {
	states := map[uint64]*ParentForkState{}
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
			if states[state.Plan.Sequence] != nil {
				return nil, errors.New("duplicate parallel fork plan")
			}
			states[state.Plan.Sequence] = state
		case ParentForkReadyKind, ParentForkRootReadyKind:
			if err := consumeParentForkReady(states, event); err != nil {
				return nil, err
			}
		}
	}
	return states, nil
}

func readParentForkPlan(reader *journal.Reader, event journal.Event) (*ParentForkState, error) {
	var ref journal.Ref
	data, err := json.Marshal(event.Runner["plan"])
	if err != nil || json.Unmarshal(data, &ref) != nil || ref.Integrity != apiv1.IntegrityTrusted {
		return nil, errors.New("invalid parallel fork plan reference")
	}
	data, err = reader.ArtifactBytesBounded(ref, MaxParentForkPlanBytes)
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
	if err := validateParentForkOwner(plan, id); err != nil {
		return nil, err
	}
	if !blobstore.ValidDigest(plan.Source.Metadata.Digest) || plan.Source.Metadata.Size <= 0 || plan.Source.Metadata.Size > MaxParentForkPlanBytes || !blobstore.ValidDigest(plan.Source.Bundle.Digest) || plan.Source.Bundle.Size <= 0 || plan.Source.Bundle.Size > MaxParentForkBundleBytes {
		return nil, errors.New("parallel fork source identity changed")
	}

	return &ParentForkState{Plan: plan, Reference: ref, PlannedAt: event.Seq, Ready: map[int]bool{}}, nil
}

func consumeParentForkReady(states map[uint64]*ParentForkState, event journal.Event) error {
	value, err := decodeParentForkCustody(event)
	if err != nil {
		return err
	}
	state := states[value.Sequence]
	if state == nil {
		return errors.New("parallel fork readiness has no durable plan")
	}
	owner, valid := state.Plan.Workspace(event.Branch)
	if !valid || event.Parallel != state.Plan.Parallel || event.Seq <= state.PlannedAt || value.PlannedAt != state.PlannedAt || state.Ready[event.Branch] || !reflect.DeepEqual(state.Reference, value.Plan) || value.Workspace != owner {
		return errors.New("parallel fork readiness differs from durable plan")
	}
	state.Ready[event.Branch] = true
	return nil
}
