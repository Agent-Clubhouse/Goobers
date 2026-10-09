package runner

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
	Archive        journal.Ref                     `json:"archive"`
	HoldSeq        uint64                          `json:"holdSeq"`
	ReturnSeq      uint64                          `json:"returnSeq"`
}

type parentWorkspaceState struct {
	hold                          ContainedParentWorkspaceCustody
	contribution                  parentContribution
	heldAt, returnedAt, retiredAt uint64
	archive                       ParentWorkspaceArchive
	branch                        int
	restoredRetirement            uint64
}

func readParentWorkspaceState(events []journal.Event, runID, branch string) (parentWorkspaceState, error) {
	var state parentWorkspaceState
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		if err := state.consume(event, runID, branch); err != nil {
			return state, err
		}
	}
	if state.heldAt != 0 && state.returnedAt <= state.heldAt {
		return state, errors.New("parent workspace has no acknowledged return for its latest owner")
	}
	return state, nil
}

func (s *parentWorkspaceState) consume(event journal.Event, runID, branch string) error {
	switch event.Runner["kind"] {
	case ContainedParentWorkspaceKind:
		return s.consumeHold(event, runID, branch)
	case ParentContributionKind:
		return s.consumeReturn(event, runID, branch)
	case ParentContributionRetiredKind, ParentContributionRestoredKind:
		return s.consumeArchive(event, runID, branch)
	}
	return nil
}

func (s *parentWorkspaceState) consumeHold(event journal.Event, runID, branch string) error {
	var value ContainedParentWorkspaceCustody
	data, err := json.Marshal(event.Runner["custody"])
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &value) != nil || value.Version != 1 || value.Origin == nil || value.Workspace.OwnerRunID != runID {
		return errors.New("invalid retained parent workspace")
	}
	if value.Workspace.Branch != branch {
		return nil
	}
	if s.retiredAt != 0 {
		return errors.New("parent workspace was reused before restoration")
	}
	s.hold, s.heldAt = value, event.Seq
	s.branch, s.restoredRetirement = event.Branch, 0
	return nil
}

func (s *parentWorkspaceState) consumeReturn(event journal.Event, runID, branch string) error {
	value, err := decodeParentContribution(event)
	if err != nil {
		return err
	}
	if value.Custody.Workspace.OwnerRunID != runID {
		return errors.New("parent contribution belongs to another run")
	}
	if value.Custody.Workspace.Branch != branch {
		return nil
	}
	if s.retiredAt != 0 || s.heldAt == 0 || event.Branch != s.branch || event.Seq <= s.heldAt || value.Custody.Workspace != s.hold.Workspace || *value.Custody.Origin != *s.hold.Origin {
		return errors.New("parent contribution differs from retained workspace")
	}
	s.contribution, s.returnedAt = value, event.Seq
	return nil
}

func (s *parentWorkspaceState) consumeArchive(event journal.Event, runID, branch string) error {
	value, err := decodeParentWorkspaceArchive(event)
	if err != nil {
		return err
	}
	if value.Custody.Workspace.OwnerRunID != runID {
		return errors.New("parent archive belongs to another run")
	}
	if value.Custody.Workspace.Branch != branch {
		return nil
	}
	if value.HoldSeq != s.heldAt || value.ReturnSeq != s.returnedAt || event.Branch != s.branch || event.Seq <= s.returnedAt || !reflect.DeepEqual(value.contribution(), s.contribution) {
		return errors.New("parent archive differs from latest acknowledged workspace")
	}
	if event.Runner["kind"] == ParentContributionRetiredKind {
		if s.retiredAt != 0 {
			return errors.New("parent workspace already has unresolved retirement")
		}
		s.archive, s.retiredAt = value, event.Seq
		return nil
	}
	var restored struct {
		RetirementSeq uint64 `json:"retirementSeq"`
	}
	data, err := json.Marshal(event.Runner)
	if err != nil || json.Unmarshal(data, &restored) != nil || s.retiredAt == 0 || restored.RetirementSeq != s.retiredAt || event.Seq <= s.retiredAt || !reflect.DeepEqual(value, s.archive) {
		return errors.New("parent restoration does not match outstanding retirement")
	}
	s.restoredRetirement, s.retiredAt = s.retiredAt, 0
	return nil
}

func (a ParentWorkspaceArchive) contribution() parentContribution {
	return parentContribution{Version: 1, ContractDigest: a.ContractDigest, Custody: a.Custody, Output: a.Output}
}

func decodeParentWorkspaceArchive(event journal.Event) (ParentWorkspaceArchive, error) {
	var value ParentWorkspaceArchive
	data, err := json.Marshal(event.Runner["archive"])
	if err != nil || len(data) > 16384 || json.Unmarshal(data, &value) != nil || value.Version != 1 || value.HoldSeq == 0 || value.ReturnSeq <= value.HoldSeq {
		return value, errors.New("invalid parent workspace archive")
	}
	if value.Archive.Path == "" || value.Archive.Size <= 0 || value.Archive.Size > 16384 || !blobstore.ValidDigest(value.Archive.Digest) {
		return value, errors.New("invalid parent archive artifact")
	}
	_, err = decodeParentContribution(journal.Event{Runner: map[string]any{"contractDigest": value.ContractDigest, "contribution": value.contribution()}})
	if err != nil {
		return value, errors.New("invalid parent archive provenance")
	}
	return value, nil
}
