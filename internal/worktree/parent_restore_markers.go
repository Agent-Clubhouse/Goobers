package worktree

import (
	"errors"
	"os"
	"path/filepath"
)

// Creation records ownership before creating Git's checkout, and its original
// revision afterwards. Build a candidate pair from surviving host metadata;
// adoptParentRestore verifies the archive's exact branch/HEAD/registration
// before persisting either repair. With no surviving record, refuse adoption.
func (wt *Worktree) parentRestoreMarkers(opts CreateOptions) (marker, marker, error) {
	primary, primaryErr := readMarker(wt.manager.markerPath(wt.key, wt.RunID))
	ownership, ownershipErr := readMarker(wt.manager.ownershipPath(wt.key, filepath.Base(wt.Path)))
	if errors.Is(primaryErr, os.ErrNotExist) && ownershipErr == nil {
		primary, primaryErr = ownership, nil
	}
	if errors.Is(ownershipErr, os.ErrNotExist) && primaryErr == nil {
		ownership, ownershipErr = primary, nil
	}
	if primaryErr != nil || ownershipErr != nil {
		return marker{}, marker{}, errors.Join(primaryErr, ownershipErr)
	}
	if primary.StartRef == "" {
		primary.StartRef = opts.parentRestoreStart
	}
	if ownership.StartRef == "" {
		ownership.StartRef = opts.parentRestoreStart
	}
	custody := StageCustody{WorkspaceID: wt.RunID, OwnerRunID: opts.OwnerRunID, RepositoryDigest: RepositoryDigest(opts.RepoURL), Branch: opts.Branch, StartRef: opts.parentRestoreStart}
	if err := archivedStageMarkers(primary, ownership, custody, filepath.Base(wt.Path)); err != nil {
		return marker{}, marker{}, err
	}
	return primary, ownership, nil
}
