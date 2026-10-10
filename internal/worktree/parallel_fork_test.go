package worktree

import (
	"strings"
	"testing"
)

func TestParallelForkIdentityBindsVisitBranchAndOwner(t *testing.T) {
	valid := ParallelForkOptions{RepoURL: "repository", OwnerRunID: "parent", Gaggle: "web", ParallelSequence: 1, Branch: 1, SnapshotSHA: strings.Repeat("a", 40)}
	first, err := parallelForkCreateOptions(valid)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{first.RunID: true}
	for _, changed := range []ParallelForkOptions{
		{RepoURL: valid.RepoURL, OwnerRunID: "other", Gaggle: "web", ParallelSequence: 1, Branch: 1, SnapshotSHA: valid.SnapshotSHA},
		{RepoURL: valid.RepoURL, OwnerRunID: "parent", Gaggle: "other", ParallelSequence: 1, Branch: 1, SnapshotSHA: valid.SnapshotSHA},
		{RepoURL: valid.RepoURL, OwnerRunID: "parent", Gaggle: "web", ParallelSequence: 2, Branch: 1, SnapshotSHA: valid.SnapshotSHA},
		{RepoURL: valid.RepoURL, OwnerRunID: "parent", Gaggle: "web", ParallelSequence: 1, Branch: 2, SnapshotSHA: valid.SnapshotSHA},
	} {
		create, err := parallelForkCreateOptions(changed)
		if err != nil || seen[create.RunID] || !create.RetainOnCleanup || create.OwnerRunID != changed.OwnerRunID {
			t.Fatal("fork identity did not bind exact owner", create, err)
		}
		seen[create.RunID] = true
	}
	changed := valid
	changed.SnapshotSHA = strings.Repeat("b", 40)
	otherSource, err := parallelForkCreateOptions(changed)
	if err != nil || otherSource.RunID != first.RunID || otherSource.Branch != first.Branch {
		t.Fatal("changed source bypasses existing fork custody", err)
	}
	for _, mutate := range []func(*ParallelForkOptions){
		func(o *ParallelForkOptions) { o.OwnerRunID = "../parent" },
		func(o *ParallelForkOptions) { o.OwnerRunID = strings.Repeat("x", 257) },
		func(o *ParallelForkOptions) { o.Gaggle = "" },
		func(o *ParallelForkOptions) { o.ParallelSequence = 0 },
		func(o *ParallelForkOptions) { o.Branch = 0 },
		func(o *ParallelForkOptions) { o.Branch = 129 },
		func(o *ParallelForkOptions) { o.SnapshotSHA = "HEAD" },
	} {
		invalid := valid
		mutate(&invalid)
		if _, err := parallelForkCreateOptions(invalid); err == nil {
			t.Fatal("invalid fork custody accepted", invalid)
		}
	}
}
