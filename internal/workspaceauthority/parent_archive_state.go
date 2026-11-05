package workspaceauthority

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

// ParentContributionRestoredKind acknowledges verified archive application and
// renewed custody, before any resumed stage may use the checkout.
const ParentContributionRestoredKind = "isolated.parent.contribution.restored"

// ParentWorkspaceArchive binds a retained archive to one exact checkout owner
// and acknowledged return. Archive references bounded host-authored recovery
// metadata in the run journal, never an arbitrary inventory/filesystem path.
type ParentWorkspaceArchive struct {
	Version        int                             `json:"version"`
	Custody        ContainedParentWorkspaceCustody `json:"custody"`
	ContractDigest string                          `json:"contractDigest"`
	Output         journal.Ref                     `json:"output"`
	Fork           *ParentForkCustody              `json:"fork,omitempty"`
	Archive        journal.Ref                     `json:"archive"`
	HoldSeq        uint64                          `json:"holdSeq"`
	ReturnSeq      uint64                          `json:"returnSeq"`
}

// ErrParentReturnPending keeps recovery custody pinned until the latest writer returns.
var ErrParentReturnPending = errors.New("parent workspace has no acknowledged return for its latest owner")

// ParentWorkspaceState tracks custody, return, retirement and restoration for one workspace.
type ParentWorkspaceState struct {
	Hold                          ContainedParentWorkspaceCustody
	Contribution                  ParentContribution
	Fork                          *ParentForkCustody
	HeldAt, ReturnedAt, RetiredAt uint64
	Archive                       ParentWorkspaceArchive
	Branch                        int
	RestoredRetirement            uint64
}

// ReadParentWorkspaceState replays workspace receipts and refuses an unacknowledged owner.
func ReadParentWorkspaceState(events []journal.Event, runID, branch string) (ParentWorkspaceState, error) {
	var state ParentWorkspaceState
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		if err := state.consume(event, runID, branch); err != nil {
			return state, err
		}
	}
	if state.HeldAt != 0 && state.ReturnedAt <= state.HeldAt {
		return state, ErrParentReturnPending
	}
	return state, nil
}

func (s *ParentWorkspaceState) consume(event journal.Event, runID, branch string) error {
	switch event.Runner["kind"] {
	case ParentForkReadyKind, ParentForkRootReadyKind:
		return s.consumeFork(event, runID, branch)
	case ContainedParentWorkspaceKind:
		return s.consumeHold(event, runID, branch)
	case ParentContributionKind:
		return s.consumeReturn(event, runID, branch)
	case ParentContributionRetiredKind, ParentContributionRestoredKind:
		return s.consumeArchive(event, runID, branch)
	}
	return nil
}

func (s *ParentWorkspaceState) consumeHold(event journal.Event, runID, branch string) error {
	var value ContainedParentWorkspaceCustody
	data, err := json.Marshal(event.Runner["custody"])
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &value) != nil || value.Version != 1 || value.Origin == nil || value.Workspace.OwnerRunID != runID {
		return errors.New("invalid retained parent workspace")
	}
	if value.Workspace.Branch != branch {
		return nil
	}
	if s.RetiredAt != 0 {
		return errors.New("parent workspace was reused before restoration")
	}
	s.Fork = nil
	s.Hold, s.HeldAt = value, event.Seq
	s.Branch, s.RestoredRetirement = event.Branch, 0
	return nil
}

func (s *ParentWorkspaceState) consumeReturn(event journal.Event, runID, branch string) error {
	value, err := DecodeParentContribution(event)
	if err != nil {
		return err
	}
	if value.Custody.Workspace.OwnerRunID != runID {
		return errors.New("parent contribution belongs to another run")
	}
	if value.Custody.Workspace.Branch != branch {
		return nil
	}
	if s.RetiredAt != 0 || s.HeldAt == 0 || event.Branch != s.Branch || event.Seq <= s.HeldAt || value.Custody.Workspace != s.Hold.Workspace || !reflect.DeepEqual(value.Custody.Origin, s.Hold.Origin) || s.Fork != nil {
		return errors.New("parent contribution differs from retained workspace")
	}
	s.Contribution, s.ReturnedAt = value, event.Seq
	return nil
}

func (s *ParentWorkspaceState) consumeArchive(event journal.Event, runID, branch string) error {
	value, err := DecodeParentWorkspaceArchive(event)
	if err != nil {
		return err
	}
	if value.Custody.Workspace.OwnerRunID != runID {
		return errors.New("parent archive belongs to another run")
	}
	if value.Custody.Workspace.Branch != branch {
		return nil
	}
	if value.HoldSeq != s.HeldAt || value.ReturnSeq != s.ReturnedAt || event.Branch != s.Branch || event.Seq <= s.ReturnedAt || !s.MatchesArchiveSource(value) {
		return errors.New("parent archive differs from latest acknowledged workspace")
	}
	if event.Runner["kind"] == ParentContributionRetiredKind {
		if s.RetiredAt != 0 {
			return errors.New("parent workspace already has unresolved retirement")
		}
		s.Archive, s.RetiredAt = value, event.Seq
		return nil
	}
	var restored struct {
		RetirementSeq uint64 `json:"retirementSeq"`
	}
	data, err := json.Marshal(event.Runner)
	if err != nil || json.Unmarshal(data, &restored) != nil || s.RetiredAt == 0 || restored.RetirementSeq != s.RetiredAt || event.Seq <= s.RetiredAt || !reflect.DeepEqual(value, s.Archive) {
		return errors.New("parent restoration does not match outstanding retirement")
	}
	s.RestoredRetirement, s.RetiredAt = s.RetiredAt, 0
	return nil
}

func (a ParentWorkspaceArchive) contribution() ParentContribution {
	return ParentContribution{Version: 1, ContractDigest: a.ContractDigest, Custody: a.Custody, Output: a.Output}
}

// DecodeParentWorkspaceArchive validates a bounded archive receipt and its source provenance.
func DecodeParentWorkspaceArchive(event journal.Event) (ParentWorkspaceArchive, error) {
	var value ParentWorkspaceArchive
	data, err := json.Marshal(event.Runner["archive"])
	if err != nil || len(data) > 16384 || json.Unmarshal(data, &value) != nil || value.Version != 1 || value.HoldSeq == 0 || value.ReturnSeq <= value.HoldSeq {
		return value, errors.New("invalid parent workspace archive")
	}
	if value.Archive.Path == "" || value.Archive.Size <= 0 || value.Archive.Size > 16384 || !blobstore.ValidDigest(value.Archive.Digest) {
		return value, errors.New("invalid parent archive artifact")
	}
	err = ValidateParentArchiveSource(value)
	if err != nil {
		return value, errors.New("invalid parent archive provenance")
	}
	return value, nil
}
