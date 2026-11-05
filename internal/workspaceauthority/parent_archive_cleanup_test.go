package workspaceauthority

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

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
