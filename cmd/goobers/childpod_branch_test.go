package main

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestChildPodPhysicalOriginSeparatesStaticBranches(t *testing.T) {
	for _, review := range []bool{false, true} {
		t.Run(map[bool]string{false: "task", true: "reviewer"}[review], func(t *testing.T) {
			assertChildPodPhysicalOrigin(t, review)
		})
	}
}

func assertChildPodPhysicalOrigin(t *testing.T, review bool) {
	t.Helper()
	kind := journal.EventStageStarted
	if review {
		kind = journal.EventReviewerStarted
	}
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "branch-origin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	for _, branch := range []int{1, 2} {
		if err := run.Append(journal.Event{Type: kind, Stage: "inspect", Attempt: 1, Branch: branch}); err != nil {
			t.Fatal(err)
		}
	}
	rd, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := childPodStarted(rd, "inspect", 1, review, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := childPodStarted(rd, "inspect", 1, review, 2)
	if err != nil || first.Seq == second.Seq {
		t.Fatal(first, second, err)
	}
	if _, err := childPodStarted(rd, "inspect", 1, review, 0); err == nil {
		t.Fatal("root selected branch attempt")
	}
	if err := run.Append(journal.Event{Type: journal.EventBranchFinished, Branch: 1, Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if _, err := childPodStarted(rd, "inspect", 1, review, 1); err == nil {
		t.Fatal("settled branch retained active authority")
	}
	if _, err := childPodStarted(rd, "inspect", 1, review, 2); err != nil {
		t.Fatal("sibling settlement revoked active branch", err)
	}
}
