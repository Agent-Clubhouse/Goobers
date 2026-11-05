package workspaceauthority

import (
	"encoding/json"
	"errors"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/worktree"
)

// ParentContributionKind is a host-only receipt for a verified imported tree.
const ParentContributionKind = "isolated.parent.contribution"

// ParentContributionRetiredKind records a durable retirement authorization,
// after archive verification and before checkout removal. Cleanup is idempotent;
// the referenced output remains readable under ordinary journal retention.
const ParentContributionRetiredKind = "isolated.parent.contribution.retired"

// MaxParentContributionBytes bounds imported parent workspace artifacts.
const MaxParentContributionBytes = 24 << 20

// ParentContribution binds an imported tree artifact to verified parent custody.
type ParentContribution struct {
	Version        int                             `json:"version"`
	ContractDigest string                          `json:"contractDigest"`
	Custody        ContainedParentWorkspaceCustody `json:"custody"`
	Output         journal.Ref                     `json:"output"`
}

// DecodeParentContribution validates an imported tree receipt and its bounded artifact reference.
func DecodeParentContribution(event journal.Event) (ParentContribution, error) {
	var result ParentContribution
	raw, err := json.Marshal(event.Runner["contribution"])
	if err != nil || len(raw) > 8192 || json.Unmarshal(raw, &result) != nil || result.Version != 1 || !blobstore.ValidDigest(result.ContractDigest) || result.ContractDigest != event.Runner["contractDigest"] || result.Custody.Version != 1 || result.Custody.Origin == nil || result.Output.Path == "" || result.Output.Size <= 0 || result.Output.Size > MaxParentContributionBytes {
		return result, errors.New("invalid parent contribution receipt")
	}
	return result, nil
}

// ParentCleanupWorkspace resolves the exact current source authority for either
// a returned contained workspace or a host-created fork. Content is always
// captured from the live checkout, never from the source artifact.
func ParentCleanupWorkspace(reader *journal.Reader, target worktree.CleanupTarget) (ParentWorkspaceArchive, bool, error) {
	var empty ParentWorkspaceArchive
	id, err := reader.Identity()
	if err != nil || id.RunID != target.OwnerRunID || id.Child != nil {
		return empty, false, errors.Join(errors.New("parent cleanup owner mismatch"), err)
	}
	events, err := reader.Events()
	if err != nil {
		return empty, false, err
	}
	pending, err := spec.PendingJoins(reader)
	if err != nil {
		return empty, false, err
	}
	if len(pending) != 0 {
		return empty, false, ErrParentReturnPending
	}
	if err := refusePendingForkCleanup(reader, events, target); err != nil {
		return empty, false, err
	}
	var branch string
	for _, event := range events {
		value, relevant, err := ParentEventCustody(event)
		if err != nil {
			return empty, false, err
		}
		if relevant && value.Workspace.WorkspaceID == target.WorktreeID {
			if value.Workspace.OwnerRunID != id.RunID {
				return empty, false, errors.New("parent cleanup workspace owner changed")
			}
			branch = value.Workspace.Branch
		}
	}
	if branch == "" {
		return empty, false, nil
	}
	state, err := ReadParentWorkspaceState(events, id.RunID, branch)
	if err != nil {
		return empty, false, err
	}
	if state.ReturnedAt == 0 || state.RetiredAt != 0 {
		return empty, false, errors.New("parent cleanup has no current acknowledged workspace")
	}
	owner := state.Hold.Workspace
	if target.Pinned || owner.WorkspaceID != target.WorktreeID || owner.RepositoryDigest != target.RepositoryDigest || owner.StartRef != target.StartRef {
		return empty, false, errors.New("parent cleanup checkout identity changed")
	}
	return state.ArchiveValue(journal.Ref{}), true, nil
}
