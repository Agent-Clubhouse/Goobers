package workspaceauthority

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
	if value.Sequence == 0 || value.PlannedAt <= value.Sequence || value.PlannedAt >= event.Seq || !validForkReceiptRole(event) || value.Plan.Size <= 0 || value.Plan.Size > MaxParentForkPlanBytes || !blobstore.ValidDigest(value.Plan.Digest) || value.Plan.Path == "" || value.Workspace.OwnerRunID == "" || value.Workspace.WorkspaceID == "" || value.Workspace.Branch == "" || value.Workspace.StartRef == "" || !blobstore.ValidDigest("sha256:"+value.Workspace.RepositoryDigest) {
		return value, errors.New("invalid parallel fork custody")
	}
	return value, nil
}

func (s *ParentWorkspaceState) consumeFork(event journal.Event, runID, branch string) error {
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
	if s.RetiredAt != 0 || (s.HeldAt != 0 && (event.Branch != 0 || s.Branch != 0 || s.ReturnedAt <= s.HeldAt || s.Hold.Workspace != value.Workspace)) {
		return errors.New("parallel fork custody was already established or not returned")
	}
	s.Contribution = ParentContribution{}
	s.Archive, s.RestoredRetirement = ParentWorkspaceArchive{}, 0
	s.Fork = &value
	s.Hold = ContainedParentWorkspaceCustody{Version: 1, Workspace: value.Workspace}
	s.HeldAt, s.ReturnedAt, s.Branch = value.PlannedAt, event.Seq, event.Branch
	return nil
}

// ArchiveValue binds an archive reference to the current acknowledged workspace state.
func (s ParentWorkspaceState) ArchiveValue(ref journal.Ref) ParentWorkspaceArchive {
	return ParentWorkspaceArchive{Version: 1, Custody: s.Hold, ContractDigest: s.Contribution.ContractDigest, Output: s.Contribution.Output, Fork: s.Fork, Archive: ref, HoldSeq: s.HeldAt, ReturnSeq: s.ReturnedAt}
}

// MatchesArchiveSource compares a receipt with the current workspace custody and contribution.
func (s ParentWorkspaceState) MatchesArchiveSource(value ParentWorkspaceArchive) bool {
	return reflect.DeepEqual(value.Custody, s.Hold) && reflect.DeepEqual(value.Fork, s.Fork) && value.ContractDigest == s.Contribution.ContractDigest && value.Output == s.Contribution.Output
}

// ValidateParentArchiveSource refuses mixed or inconsistent workspace archive provenance.
func ValidateParentArchiveSource(value ParentWorkspaceArchive) error {
	if value.Fork == nil {
		_, err := DecodeParentContribution(journal.Event{Runner: map[string]any{"contractDigest": value.ContractDigest, "contribution": value.contribution()}})
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
