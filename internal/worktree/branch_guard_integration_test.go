//go:build integration

package worktree

import (
	"context"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationUnoccupiedBranchRefusesActualManagedCheckout(t *testing.T) {
	testdep.Require(t, "git")
	repo := newSourceRepo(t)
	m := newTestManager(t)
	wt, err := m.Create(t.Context(), CreateOptions{RepoURL: repo, RunID: "stage", OwnerRunID: "owner", BaseRef: "main", Branch: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = m.WithUnoccupiedBranch(t.Context(), repo, "fix", func(context.Context) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("occupied branch admitted", err)
	}
	if err = wt.Remove(t.Context(), RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = m.WithUnoccupiedBranch(t.Context(), repo, "fix", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
