package runner

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// ParentForkCustody is the host's acquired checkout authority. It carries no
// agent origin or worker return: the referenced root plan owns its source.
type ParentForkCustody struct {
	Sequence  uint64                `json:"sequence"`
	PlannedAt uint64                `json:"plannedAt"`
	Plan      journal.Ref           `json:"plan"`
	Workspace worktree.StageCustody `json:"workspace"`
}

func decodeParentForkCustody(event journal.Event) (ParentForkCustody, error) {
	var value ParentForkCustody
	data, err := json.Marshal(event.Runner)
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &value) != nil {
		return value, errors.New("invalid parallel fork readiness")
	}
	if value.Sequence == 0 || value.PlannedAt <= value.Sequence || value.PlannedAt >= event.Seq || !validForkReceiptRole(event) || value.Plan.Size <= 0 || value.Plan.Size > maxParentForkPlanBytes || !blobstore.ValidDigest(value.Plan.Digest) || value.Plan.Path == "" || value.Workspace.OwnerRunID == "" || value.Workspace.WorkspaceID == "" || value.Workspace.Branch == "" || value.Workspace.StartRef == "" || !blobstore.ValidDigest("sha256:"+value.Workspace.RepositoryDigest) {
		return value, errors.New("invalid parallel fork custody")
	}
	return value, nil
}

func (s *parentWorkspaceState) consumeFork(event journal.Event, runID, branch string) error {
	value, err := decodeParentForkCustody(event)
	if err != nil {
		return err
	}
	if value.Workspace.OwnerRunID != runID {
		return errors.New("parallel fork belongs to another parent")
	}
	if value.Workspace.Branch != branch {
		return nil
	}
	if s.retiredAt != 0 || (s.heldAt != 0 && (event.Branch != 0 || s.branch != 0 || s.returnedAt <= s.heldAt || s.hold.Workspace != value.Workspace)) {
		return errors.New("parallel fork custody was already established or not returned")
	}
	s.contribution = parentContribution{}
	s.archive, s.restoredRetirement = ParentWorkspaceArchive{}, 0
	s.fork = &value
	s.hold = ContainedParentWorkspaceCustody{Version: 1, Workspace: value.Workspace}
	s.heldAt, s.returnedAt, s.branch = value.PlannedAt, event.Seq, event.Branch
	return nil
}

func (s parentWorkspaceState) archiveValue(ref journal.Ref) ParentWorkspaceArchive {
	return ParentWorkspaceArchive{Version: 1, Custody: s.hold, ContractDigest: s.contribution.ContractDigest, Output: s.contribution.Output, Fork: s.fork, Archive: ref, HoldSeq: s.heldAt, ReturnSeq: s.returnedAt}
}

func (s parentWorkspaceState) matchesArchiveSource(value ParentWorkspaceArchive) bool {
	return reflect.DeepEqual(value.Custody, s.hold) && reflect.DeepEqual(value.Fork, s.fork) && value.ContractDigest == s.contribution.ContractDigest && value.Output == s.contribution.Output
}

func validateParentArchiveSource(value ParentWorkspaceArchive) error {
	if value.Fork == nil {
		_, err := decodeParentContribution(journal.Event{Runner: map[string]any{"contractDigest": value.ContractDigest, "contribution": value.contribution()}})
		return err
	}
	if value.Custody.Version != 1 || value.Custody.Origin != nil || value.Custody.Workspace != value.Fork.Workspace || value.ContractDigest != "" || value.Output != (journal.Ref{}) || value.HoldSeq != value.Fork.PlannedAt {
		return errors.New("fork archive contains mixed source authority")
	}
	// Validate the receipt shape; its root plan and actual branch attribution are
	// checked against the journal before the host grants any archive authority.
	data, err := json.Marshal(value.Fork)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, err = decodeParentForkCustody(journal.Event{Branch: 1, Seq: value.ReturnSeq, Runner: fields})
	return err
}

// ParentForkArchivePlan verifies the exact host plan, branch-ready receipt and
// latest workspace source before the daemon derives archive policy or restores.
func ParentForkArchivePlan(reader *journal.Reader, archive ParentWorkspaceArchive) (ParentForkPlan, int, error) {
	var empty ParentForkPlan
	if archive.Fork == nil || validateParentArchiveSource(archive) != nil {
		return empty, 0, errors.New("fork archive lacks its exact host source")
	}
	events, err := reader.Events()
	if err != nil {
		return empty, 0, err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return empty, 0, err
	}
	fork := states[archive.Fork.Sequence]
	if fork == nil || fork.reference != archive.Fork.Plan || fork.plannedAt != archive.HoldSeq {
		return empty, 0, errors.New("fork archive plan changed")
	}
	state, err := readParentWorkspaceState(events, archive.Custody.Workspace.OwnerRunID, archive.Custody.Workspace.Branch)
	if err != nil {
		return empty, 0, err
	}
	owner, exists := fork.plan.Workspace(state.branch)
	if !state.matchesArchiveSource(archive) || state.returnedAt != archive.ReturnSeq || !exists || !fork.ready[state.branch] || owner != archive.Custody.Workspace {
		return empty, 0, errors.New("fork archive is not the current acknowledged workspace")
	}
	return fork.plan, state.branch, nil
}
