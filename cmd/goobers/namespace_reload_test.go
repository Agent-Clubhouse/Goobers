package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// namespace_reload_test.go is #4405's regression evidence: a config reload
// that changes spec.branchNamespace reuses the gaggle's existing
// worktree.Manager (buildRunnerConfig's `if wtMgr == nil` branch is skipped),
// and until this fix, WithRunBranchNamespaces — the ONLY thing that ever set
// the reused Manager's mirror-fetch prune exclusion — was applied EXCLUSIVELY
// inside that same skipped branch. A later mirror refresh, using the STALE
// exclusion, pruned the new-namespace run branch mid-run: the implementer's
// next ordinary commit became a parentless root commit, and the review
// stage's own refresh/recreate cycle silently replaced it with an empty
// branch off main, misreporting real committed work as "no changes".
//
// The fix (cmd/goobers/runnerwiring.go's unconditional
// wtMgr.SetRunBranchNamespaces call, internal/worktree/manager.go's new
// accumulate-not-replace setter) is exercised end to end here through the
// SAME seam config reload actually uses: two real calls to buildRunnerConfig
// sharing one Manager, then real Manager.Create/WorkingCopy/
// HasCommitsAheadOf/Diff calls — no fakes, no network.
//
// This is promoted from the issue's own included diagnostic, extended with a
// fourth case (custom-to-different-custom) the issue's three cases didn't
// cover, and inverted from "assert the known-bad behavior" to "assert the
// fix": every case must now preserve ancestry, none may lose it.
func TestNamespaceReloadPreservesInFlightRunBranches(t *testing.T) {
	for _, tc := range []struct {
		name      string
		initialNS string
		runNS     string
	}{
		{"default_unchanged", "goobers/", "goobers/"},
		{"custom_cold_start", "goobers-jeffstei/", "goobers-jeffstei/"},
		{"default_to_custom_hot_reload", "goobers/", "goobers-jeffstei/"},
		{"custom_to_different_custom_hot_reload", "goobers-jeffstei/", "goobers-otherteam/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			origin := initBareOrigin(t)
			previousCloneURL := repoCloneURL
			repoCloneURL = func(apiv1.RepoRef) (string, error) { return origin, nil }
			t.Cleanup(func() { repoCloneURL = previousCloneURL })
			layout := instance.NewLayout(t.TempDir())
			if err := layout.EnsureGaggleRuntime("example"); err != nil {
				t.Fatal(err)
			}
			layout = layout.ForGaggle("example")
			project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
			cfg := &instance.Config{Repos: []instance.RepoRef{{
				Provider: "github", Owner: "acme", Name: "web",
				PathLength: &instance.RepoPathLengthConfig{Disabled: true},
			}}}
			build := func(previous *worktree.Manager, ns string) (*worktree.Manager, string) {
				t.Helper()
				rc, manager, err := buildRunnerConfig(runnerCompositionInput{
					Layout: layout, Config: cfg, GaggleProject: project,
					SharedRegistry:   journal.NewRegistryScrubber(),
					WorktreeManager:  previous,
					BranchNamespaces: map[string]string{"example": ns},
					SandboxPosture:   instance.SandboxDisabled,
				})
				if err != nil {
					t.Fatal(err)
				}
				return manager, rc.BranchNamespaces["example"]
			}

			// Simulates the daemon's own startup construction, then a config
			// reload that changes (or, in the control cases, repeats) the
			// gaggle's branchNamespace — reusing the SAME Manager instance
			// both times, exactly as configReloader.poll does.
			manager, _ := build(nil, tc.initialNS)
			reloaded, ns := build(manager, tc.runNS)
			if reloaded != manager {
				t.Fatal("reload did not reuse the manager; this test no longer exercises #4405's reused-manager path")
			}

			branch := providers.BranchNameIn(ns, "implementation", "repro")
			wt, err := reloaded.Create(ctx, worktree.CreateOptions{
				RepoURL: origin, RunID: "repro-implement", OwnerRunID: "repro",
				BaseRef: "main", Branch: branch,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = wt.Remove(ctx, worktree.RemoveOptions{}) })
			base := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "main"))
			if head, err := wt.HeadSHA(ctx); err != nil || head != base {
				t.Fatalf("initial branch ancestry: HEAD=%s main=%s err=%v", head, base, err)
			}

			// The step that reproduced the incident: a SIBLING mirror refresh
			// (e.g. another concurrent run's own Create, or a retention
			// sweep) mid-run, using the manager's CURRENT prune exclusion.
			if _, err := reloaded.WorkingCopy(ctx, origin); err != nil {
				t.Fatal(err)
			}
			head, err := wt.HeadSHA(ctx)
			if err != nil {
				t.Fatalf("HEAD lost after a sibling mirror refresh — the new-namespace branch was pruned: %v", err)
			}
			if head != base {
				t.Fatalf("HEAD = %s after sibling refresh, want unchanged %s", head, base)
			}

			if err := os.WriteFile(filepath.Join(wt.Path, "README.md"), []byte("implemented change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGitT(t, wt.Path, "add", "README.md")
			runGitT(t, wt.Path, "commit", "--no-verify", "-m", "implementation")
			parents := strings.TrimSpace(runGitOutputT(t, wt.Path, "show", "-s", "--format=%P", "HEAD"))
			if parents == "" {
				t.Fatal("commit has no parent — its branch was pruned and silently recreated as an orphan root")
			}

			ahead, err := wt.HasCommitsAheadOf(ctx, "main")
			if err != nil || !ahead {
				t.Fatalf("HasCommitsAheadOf(main) = %v, %v, want true, nil", ahead, err)
			}
			diff, err := wt.Diff(ctx, "main")
			if err != nil {
				t.Fatalf("Diff(main): %v — a healthy ancestry must never hit \"no merge base\"", err)
			}
			if len(diff) == 0 {
				t.Fatal("diff is empty despite a real committed change")
			}
			if err := wt.Remove(ctx, worktree.RemoveOptions{}); err != nil {
				t.Fatal(err)
			}

			// The review stage's own Create, on the SAME branch — this is
			// where the incident's silent misattribution actually surfaced:
			// a pruned-and-recreated branch reads to the reviewer as HEAD ==
			// main, zero-byte diff, "the implementer produced nothing".
			review, err := reloaded.Create(ctx, worktree.CreateOptions{
				RepoURL: origin, RunID: "repro-review", OwnerRunID: "repro",
				BaseRef: "main", Branch: branch,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = review.Remove(ctx, worktree.RemoveOptions{}) })
			reviewDiff, err := review.Diff(ctx, "main")
			if err != nil {
				t.Fatal(err)
			}
			if len(reviewDiff) == 0 {
				t.Fatal("reviewer sees an empty diff — the implementer's committed work was discarded")
			}
			reviewHead, err := review.HeadSHA(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if reviewHead == base {
				t.Fatal("reviewer's HEAD equals main — the branch was recreated from scratch rather than carrying the implementer's commit forward")
			}

			cmd := testgit.Command("rev-parse", "--is-shallow-repository")
			cmd.Dir = review.Path
			out, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "false" {
				t.Fatalf("expected full history: %s %v", out, err)
			}
		})
	}
}

// TestSetRunBranchNamespacesAccumulatesRatherThanReplaces is the unit-level
// proof of the design decision itself: replacing the exclusion set (the
// shape SetPathLengthLimits uses) would strand a run still in flight under
// whatever namespace it's no longer configured for — exactly the failure
// this issue reports, just moved to the other side of the swap.
func TestSetRunBranchNamespacesAccumulatesRatherThanReplaces(t *testing.T) {
	manager, err := worktree.NewManager(t.TempDir(), worktree.WithRunBranchNamespaces("goobers/"))
	if err != nil {
		t.Fatal(err)
	}
	manager.SetRunBranchNamespaces("goobers-jeffstei/")
	manager.SetRunBranchNamespaces("goobers-otherteam/")

	origin := initBareOrigin(t)
	for i, ns := range []string{"goobers/", "goobers-jeffstei/", "goobers-otherteam/"} {
		branch := providers.BranchNameIn(ns, "implementation", "still-protected")
		runID := fmt.Sprintf("run-%d", i)
		wt, err := manager.Create(context.Background(), worktree.CreateOptions{
			RepoURL: origin, RunID: runID, OwnerRunID: runID,
			BaseRef: "main", Branch: branch,
		})
		if err != nil {
			t.Fatalf("create branch under %q: %v", ns, err)
		}
		base := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "main"))
		if err := wt.Remove(context.Background(), worktree.RemoveOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.WorkingCopy(context.Background(), origin); err != nil {
			t.Fatal(err)
		}
		if !localMirrorHasBranch(t, manager, origin, branch) {
			t.Fatalf("branch under namespace %q was pruned by a later namespace's own SetRunBranchNamespaces call — accumulate semantics were violated", ns)
		}
		_ = base
	}
}

// localMirrorHasBranch checks the manager's own bare mirror clone directly,
// since these branches are local-only (never pushed to origin) exactly like
// a real in-flight run's branch.
func localMirrorHasBranch(t *testing.T, manager *worktree.Manager, repoURL, branch string) bool {
	t.Helper()
	dir, err := manager.WorkingCopy(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	cmd := testgit.Command("show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = dir
	return cmd.Run() == nil
}
