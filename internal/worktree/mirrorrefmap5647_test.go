package worktree

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestManager_WorkingCopy_PinnedToUnpinnedMigrationRefreshes is the #5647
// regression. A pinned workspace initializes its mirror with `init --bare` +
// `remote add origin`, which leaves git's default tracking refmap
// (+refs/heads/*:refs/remotes/origin/*) in remote.origin.fetch, and its pinned
// fetch populates refs/remotes/origin/*. After the repository is switched to
// unpinned workspaces, WorkingCopy reuses that same repo.git for the broad
// `fetch --prune origin +refs/*:refs/*` refresh. Without suppressing the
// inherited refmap, the prune deletes refs/remotes/origin/main (it has no
// source under the explicit refspec) and the opportunistic tracking update
// then fails with "cannot lock ref 'refs/remotes/origin/main'". The refresh
// must succeed across repeated remote advances while keeping local run and
// recovery refs.
func TestManager_WorkingCopy_PinnedToUnpinnedMigrationRefreshes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	manager, repo := pinnedFixture(t)
	lease, err := manager.AcquirePinned(ctx, PinnedOptions{
		RepoURL: repo, RunID: "pinned-run", BaseRef: "main",
		Branch: "goobers/test/pinned-run", CleanPolicy: PinnedCleanNone,
	})
	if err != nil {
		t.Fatalf("AcquirePinned: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	mirror := manager.repoDirForKey(repoKey(repo))
	// Loud preconditions: the reproduction depends on the pinned mirror
	// carrying the inherited tracking refmap and a populated tracking ref.
	if got := strings.TrimSpace(runTestGit(t, mirror, "config", "--get-all", "remote.origin.fetch")); got != "+refs/heads/*:refs/remotes/origin/*" {
		t.Fatalf("precondition: pinned mirror remote.origin.fetch = %q, want the default tracking refmap", got)
	}
	runTestGit(t, mirror, "rev-parse", "--verify", "refs/remotes/origin/main")

	head := strings.TrimSpace(runTestGit(t, mirror, "rev-parse", "refs/heads/main"))
	runBranch := "refs/heads/goobers/implementation/run1"
	recoveryRef := "refs/goobers/recovery-snapshots/run1/" + head
	runTestGit(t, mirror, "update-ref", runBranch, head)
	runTestGit(t, mirror, "update-ref", recoveryRef, head)

	for i := range 3 {
		mustWriteFile(t, filepath.Join(repo, "advance.txt"), strings.Repeat("x", i+1)+"\n")
		runTestGit(t, repo, "add", ".")
		runTestGit(t, repo, "commit", "-m", "advance origin")
		want := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "HEAD"))

		dir, err := manager.WorkingCopy(ctx, repo)
		if err != nil {
			t.Fatalf("unpinned refresh %d after pinned initialization: %v", i+1, err)
		}
		if dir != mirror {
			t.Fatalf("WorkingCopy dir = %q, want reused pinned mirror %q", dir, mirror)
		}
		if got := strings.TrimSpace(runTestGit(t, mirror, "rev-parse", "refs/heads/main")); got != want {
			t.Fatalf("refresh %d: mirror main = %s, want origin head %s", i+1, got, want)
		}
		for _, ref := range []string{runBranch, recoveryRef} {
			if got := strings.TrimSpace(runTestGit(t, mirror, "rev-parse", ref)); got != head {
				t.Fatalf("refresh %d: local ref %s = %q, want preserved at %s", i+1, ref, got, head)
			}
		}
	}
}
