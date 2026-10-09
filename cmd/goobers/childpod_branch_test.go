package main

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestChildPodPhysicalOriginSeparatesStaticBranches(t *testing.T) {
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "branch-origin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	for _, branch := range []int{1, 2} {
		if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "inspect", Attempt: 1, Branch: branch}); err != nil {
			t.Fatal(err)
		}
	}
	rd, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := childPodStarted(rd, "inspect", 1, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := childPodStarted(rd, "inspect", 1, false, 2)
	if err != nil || first.Seq == second.Seq {
		t.Fatal(first, second, err)
	}
	if _, err := childPodStarted(rd, "inspect", 1, false, 0); err == nil {
		t.Fatal("root selected branch attempt")
	}
	if err := run.Append(journal.Event{Type: journal.EventBranchFinished, Branch: 1, Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if _, err := childPodStarted(rd, "inspect", 1, false, 1); err == nil {
		t.Fatal("settled branch retained active authority")
	}
	if _, err := childPodStarted(rd, "inspect", 1, false, 2); err != nil {
		t.Fatal("sibling settlement revoked active branch", err)
	}
}
