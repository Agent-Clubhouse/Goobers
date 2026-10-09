//go:build integration

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

func forkArchiveCandidate(t *testing.T, values []runner.ParentRetirementCandidate, branch int) runner.ParentRetirementCandidate {
	t.Helper()
	for _, value := range values {
		if value.Branch == branch {
			return value
		}
	}
	t.Fatalf("missing archived branch %d", branch)
	return runner.ParentRetirementCandidate{}
}

func verifyRootForkArchiveRestore(t *testing.T, restorer parentArchiveRestorer, run *journal.Run, reader *journal.Reader, source *worktree.Worktree, candidates []runner.ParentRetirementCandidate, head, index string) {
	t.Helper()
	root := forkArchiveCandidate(t, candidates, 0)
	if root.Workspace.Custody.Origin != nil || root.Workspace.Fork == nil || root.Workspace.ContractDigest != "" {
		t.Fatal("root archive invented a worker return", root)
	}
	if _, branch, err := runner.ParentForkArchivePlan(reader, root.Workspace); err != nil || branch != 0 {
		t.Fatal("root archive lost host provenance", branch, err)
	}
	if err := source.Remove(t.Context(), worktree.RemoveOptions{}); err != nil {
		t.Fatal("root archive cleanup", err)
	}
	if _, err := os.Stat(source.Path); !os.IsNotExist(err) {
		t.Fatal("archived root was not removed", err)
	}
	branch, err := runner.OwnedBranchRecorder(run, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := restorer.restore(t.Context(), branch, root.Workspace, root.RetirementSeq); err == nil {
		t.Fatal("branch writer restored the root")
	}
	if err := restorer.restore(t.Context(), run, root.Workspace, root.RetirementSeq); err != nil {
		t.Fatal("root archive restoration", err)
	}
	if recoveryCLIGit(t, source.Path, "rev-parse", "HEAD") != head || recoveryCLIGit(t, source.Path, "write-tree") != index {
		t.Fatal("root restoration changed committed or staged state")
	}
	if got := readFileContent(t, filepath.Join(source.Path, "source.txt")); got != "later root edits\n" {
		t.Fatal("root restoration lost latest work", got)
	}
}
