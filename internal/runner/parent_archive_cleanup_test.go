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
	c := state.archive.Custody.Workspace
	target := worktree.CleanupTarget{OwnerRunID: c.OwnerRunID, WorktreeID: c.WorkspaceID, RepositoryDigest: c.RepositoryDigest, StartRef: c.StartRef}
	if got, found, err := ParentCleanupArchive(reader, target); err != nil || !found || got.Archive != record {
		t.Fatal("retirement did not authorize exact checkout", found, err)
	}
	wrong := target
	wrong.StartRef = "foreign"
	if _, _, err := ParentCleanupArchive(reader, wrong); err == nil {
		t.Fatal("foreign custody authorized")
	}
	if err := RecordParentArchiveRestoration(rec, state.archive, state.retiredAt); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ParentCleanupArchive(reader, target); err != nil || found {
		t.Fatal("old retirement still authorized cleanup after restore", found, err)
	}
}

func TestParentArchiveGuardLeavesGeneratedChildCleanupToItsOwnProtocol(t *testing.T) {
	id := journal.RunIdentity{RunID: "child-run", Gaggle: "web", Child: &journal.ChildLineage{Gaggle: "web", ParentRunID: "parent-run", ParentWorkflow: "parent-workflow", StageOccurrence: "parent-stage", InvocationKey: "check", AcceptanceID: "trigger-child-run", SourceDigest: journal.Digest([]byte("source")), EnvelopeDigest: journal.Digest([]byte("envelope"))}}
	id.ConfigGeneration = journal.Digest([]byte("generation"))
	id.WorkflowDigest = journal.Digest([]byte("workflow"))
	id.GooberDigest = journal.Digest([]byte("goober"))
	run, err := journal.Create(t.TempDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := ParentCleanupArchive(reader, worktree.CleanupTarget{OwnerRunID: id.RunID}); err != nil || found {
		t.Fatal("parent guard intercepted generated child", found, err)
	}
}
