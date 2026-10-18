//go:build integration

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParallelParentCancellationStopsBothAuthoredChildren(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	qualifyContainedParentJourney(t, "parallel-cancel")
}

func assertCancelledParallelParentsRetained(t *testing.T, f pinnedChildFixture, runID string, children []triggerqueue.ChildRecord) {
	t.Helper()
	for _, child := range children {
		if !child.AcknowledgedAt.IsZero() {
			t.Fatal("cancellation acknowledged a child without a disposition")
		}
	}
	dir, err := f.layout.FindRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyParentArchiveChildren(t.Context(), f.layout, id); !errors.Is(err, invoke.ErrChildCustodyPending) {
		t.Fatal("unacknowledged siblings no longer protect cleanup", err)
	}
	pending, err := runner.ParentRetirementCandidates(reader)
	if err != nil || len(pending) != 3 {
		t.Fatal("root or branch workspace custody missing", len(pending), err)
	}
	layout, err := instance.EffectiveWorkcopiesLayout(f.layout.ForGaggle(id.Gaggle), f.cfg, &f.applied.Gaggles[0])
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	url, err := childRepoCloneURL(f.applied.Gaggles[0].Spec.Project)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, candidate := range pending {
		if seen[candidate.Branch] || candidate.Branch < 0 || candidate.Branch > 2 || candidate.RetirementSeq != 0 {
			t.Fatal("cancelled branch custody changed or retired", candidate.Branch)
		}
		seen[candidate.Branch] = true
		checkout, err := manager.AdoptHeldStage(t.Context(), url, candidate.Workspace.Custody.Workspace)
		if err != nil {
			t.Fatal("exact branch hold lost", candidate.Branch, err)
		}
		for index, name := range []string{"left", "right"} {
			data, err := os.ReadFile(filepath.Join(checkout.Path, "parent-"+name+".txt"))
			if candidate.Branch == index+1 {
				if err != nil || string(data) != name+" before child\n" {
					t.Fatal("cancelled branch work lost", name, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("cancelled workspace contains a sibling's work", candidate.Branch, name)
			}
		}
	}
}
