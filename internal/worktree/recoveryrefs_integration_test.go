//go:build integration

package worktree

import (
	"context"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationMirrorRefreshPreservesRecoveryRefs(t *testing.T) {
	testdep.Require(t, "git")
	repo := newSourceRepo(t)
	m, err := NewManager(t.TempDir(), WithRunBranchNamespaces("acme/"))
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := m.WorkingCopy(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(runTestGit(t, mirror, "rev-parse", "refs/heads/main"))
	refs := []string{"refs/goobers/recovery/run1", "refs/goobers/recovery-snapshots/run1/" + head}
	for _, ref := range refs {
		runTestGit(t, mirror, "update-ref", ref, head)
	}
	runTestGit(t, mirror, "update-ref", "refs/ordinary/local-only", head)
	for _, remoteConflict := range []bool{false, true} {
		if remoteConflict {
			runTestGit(t, repo, "commit", "--allow-empty", "-m", "remote advance")
			for _, ref := range refs {
				runTestGit(t, repo, "update-ref", ref, "HEAD")
			}
		}
		if _, err := m.WorkingCopy(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
		for _, ref := range refs {
			got := strings.TrimSpace(runTestGit(t, mirror, "for-each-ref", "--format=%(objectname)", ref))
			if got != head {
				t.Fatalf("refresh changed recovery ref %s: got %q, want %s (remote conflict=%t)", ref, got, head, remoteConflict)
			}
		}
	}
	if got := strings.TrimSpace(runTestGit(t, mirror, "for-each-ref", "--format=%(refname)", "refs/ordinary/local-only")); got != "" {
		t.Fatal("ordinary local-only ref escaped mirror pruning")
	}
}
