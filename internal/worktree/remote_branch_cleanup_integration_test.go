//go:build integration

package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationRemoteBranchCleanupLeaseAndRecovery(t *testing.T) {
	for _, fork := range []bool{false, true} {
		for _, scenario := range []string{"delete-retry", "changed-tip", "lost-lease", "missing-evidence"} {
			name := "same/" + scenario
			if fork {
				name = "fork/" + scenario
			}
			t.Run(name, func(t *testing.T) {
				opts, source := remoteBranchFixture(t, fork)
				ctx := context.Background()
				sourceTip := runTestGit(t, opts.SourceURL, "rev-parse", "HEAD")
				if err := EstablishRemoteBranch(ctx, opts); err != nil {
					t.Fatal(err)
				}
				expected := opts.Binding.StartingSHA
				want := CleanupDeleted
				if scenario == "missing-evidence" {
					expected, want = "", CleanupOwnershipMissing
				}
				if scenario == "changed-tip" || scenario == "lost-lease" {
					runTestGit(t, source, "commit", "--allow-empty", "-m", "external owner")
					other := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
					runTestGit(t, source, "push", opts.TargetURL, other+":refs/heads/foreign")
					if scenario == "changed-tip" {
						runTestGit(t, opts.TargetURL, "update-ref", opts.Binding.Ref, other)
					} else {
						hook := "#!/bin/sh\nwhile read old new ref; do\n git update-ref \"$ref\" " + other + " \"$old\" || exit 1\ndone\n"
						if err := os.WriteFile(filepath.Join(opts.TargetURL, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil {
							t.Fatal(err)
						}
					}
					want = CleanupTipChanged
				}
				opts.SourceRead.Authorized = false
				result, err := CleanupRemoteBranch(ctx, opts, expected)
				if err != nil || result.Outcome != want {
					t.Fatalf("cleanup = %+v, %v; want %s", result, err, want)
				}
				if scenario == "delete-retry" {
					// The previous deletion's audit record could have been lost.
					result, err = CleanupRemoteBranch(ctx, opts, expected)
					if err != nil || result.Outcome != CleanupAlreadyAbsent {
						t.Fatalf("restart = %+v, %v", result, err)
					}
				}
				if after := runTestGit(t, opts.SourceURL, "rev-parse", "HEAD"); after != sourceTip {
					t.Fatal("cleanup changed original source branch")
				}
			})
		}
	}
}

func TestIntegrationLegacyPinnedInspectionDiscardsOnlyInspectionCommits(t *testing.T) {
	ctx := context.Background()
	source := newSourceRepo(t)
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquirePinned(ctx, PinnedOptions{RepoURL: source, RunID: "readonly", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Release(); err != nil {
			t.Error(err)
		}
	}()
	wt := lease.Worktree
	const branch = "goobers/inspection/run"
	if err := wt.PreparePinned(ctx, PinnedPrepareOptions{BaseRef: "main", Branch: branch}); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, wt.Path, "commit", "--allow-empty", "-m", "prior writable state")
	before := strings.TrimSpace(runTestGit(t, wt.Path, "rev-parse", "HEAD"))
	sha, err := wt.PreparePinnedInspection(ctx, nil)
	if err != nil || sha != before {
		t.Fatalf("inspection baseline %s: %v", sha, err)
	}
	runTestGit(t, wt.Path, "commit", "--allow-empty", "-m", "discard inspection commit")
	mustWriteFile(t, filepath.Join(wt.Path, "untracked"), "discard")
	mustWriteFile(t, filepath.Join(wt.Path, ".gitignore"), "*.ignored\n")
	mustWriteFile(t, filepath.Join(wt.Path, "build.ignored"), "discard")
	if err := wt.ResetPinnedRevision(ctx, sha); err != nil {
		t.Fatal(err)
	}
	if err := wt.PreparePinned(ctx, PinnedPrepareOptions{BaseRef: "main", Branch: branch}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runTestGit(t, wt.Path, "rev-parse", "HEAD")); got != before {
		t.Fatalf("readonly advanced writable state: %s", got)
	}
	for _, name := range []string{"untracked", "build.ignored"} {
		if _, err := os.Stat(filepath.Join(wt.Path, name)); !os.IsNotExist(err) {
			t.Fatalf("retained %s: %v", name, err)
		}
	}
}
