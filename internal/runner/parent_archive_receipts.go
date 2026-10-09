package runner

import (
	"errors"
	"reflect"

	"github.com/goobers/goobers/internal/journal"
)

// RecordParentArchiveRetirement records removal authority only after the host
// has verified archive custody, restorability and joined parent/child writers.
// It neither releases a hold nor removes files. An identical retry is a no-op;
// a new cycle after restoration gets a new event sequence even for identical
// archive bytes. Overflow-only retention cannot authorize removal here.
func RecordParentArchiveRetirement(rec OwnedJournalRecorder, branch string, archive journal.Ref) error {
	state, err := ownedParentArchiveState(rec, branch)
	if err != nil {
		return err
	}
	value := ParentWorkspaceArchive{Version: 1, Custody: state.contribution.Custody, ContractDigest: state.contribution.ContractDigest, Output: state.contribution.Output, Archive: archive, HoldSeq: state.heldAt, ReturnSeq: state.returnedAt}
	event := journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": ParentContributionRetiredKind, "archive": value}}
	if _, err := decodeParentWorkspaceArchive(event); err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return err
	}
	if _, err := reader.ArtifactBytesBounded(archive, 16384); err != nil {
		return err
	}
	if state.retiredAt != 0 {
		if !reflect.DeepEqual(value, state.archive) {
			return errors.New("parent retirement changed on replay")
		}
		return nil
	}
	return rec.Append(event)
}

// RecordParentArchiveRestoration follows verified application of durable host
// intent and successful reacquisition of the original managed checkout hold.
// Only the exact outstanding retirement can become runnable again.
func RecordParentArchiveRestoration(rec OwnedJournalRecorder, archive ParentWorkspaceArchive, retirementSeq uint64) error {
	state, err := ownedParentArchiveState(rec, archive.Custody.Workspace.Branch)
	if err != nil {
		return err
	}
	if state.retiredAt == 0 && retirementSeq != 0 && state.restoredRetirement == retirementSeq && reflect.DeepEqual(archive, state.archive) {
		return nil
	}
	if state.retiredAt == 0 || state.retiredAt != retirementSeq || !reflect.DeepEqual(archive, state.archive) {
		return errors.New("parent restoration differs from outstanding retirement")
	}
	return rec.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": ParentContributionRestoredKind, "archive": archive, "retirementSeq": retirementSeq}})
}

func ownedParentArchiveState(rec OwnedJournalRecorder, branch string) (parentWorkspaceState, error) {
	var empty parentWorkspaceState
	owned, branchIndex, err := OwnedJournalScope(rec)
	if err != nil {
		return empty, err
	}
	reader, err := journal.OpenReadOnly(owned.Dir())
	if err != nil {
		return empty, err
	}
	id, err := reader.Identity()
	if err != nil {
		return empty, err
	}
	if id.Child != nil || branch == "" {
		return empty, errors.New("parent archive requires an owned parent branch")
	}
	events, err := reader.Events()
	if err != nil {
		return empty, err
	}
	state, err := readParentWorkspaceState(events, id.RunID, branch)
	if err != nil {
		return state, err
	}
	if state.returnedAt == 0 || state.branch != branchIndex {
		return empty, errors.New("parent archive has no acknowledged workspace")
	}
	if state.archive.Archive.Path != "" {
		if _, err := reader.ArtifactBytesBounded(state.archive.Archive, 16384); err != nil {
			return empty, err
		}
	}
	return state, nil
}
