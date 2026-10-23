package runner

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestParentCleanupArchiveAuthorityEndsOnRestoration(t *testing.T) {
	run, rec, record := parentArchiveReceiptFixture(t, 0)
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordParentArchiveRetirement(rec, "parent-branch", record); err != nil {
		t.Fatal(err)
	}
	state, err := ownedParentArchiveState(rec, "parent-branch")
	if err != nil {
		t.Fatal(err)
	}
	c := state.Archive.Custody.Workspace
	target := worktree.CleanupTarget{OwnerRunID: c.OwnerRunID, WorktreeID: c.WorkspaceID, RepositoryDigest: c.RepositoryDigest, StartRef: c.StartRef}
	if got, found, err := ParentCleanupArchive(reader, target); err != nil || !found || got.Archive != record {
		t.Fatal("retirement did not authorize exact checkout", found, err)
	}
	wrong := target
	wrong.StartRef = "foreign"
	if _, _, err := ParentCleanupArchive(reader, wrong); err == nil {
		t.Fatal("foreign custody authorized")
	}
	if err := RecordParentArchiveRestoration(rec, state.Archive, state.RetiredAt); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ParentCleanupArchive(reader, target); err != nil || found {
		t.Fatal("old retirement still authorized cleanup after restore", found, err)
	}
}
