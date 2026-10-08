package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// A reaped worktree whose checkout directory is already gone must not defer
// forever on a handoff that can only read that directory (#5383). Its
// abandoned preparation is still handed off from the shared repository, and a
// live run's worktree is never selected.
func TestRecoveryCleanupReapsGoneCheckout(t *testing.T) {
	testdep.Require(t, "git")

	for _, tc := range []struct {
		name        string
		preparation bool
	}{
		{name: "no-preparation"},
		{name: "abandoned-preparation", preparation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := instance.NewLayout(initDemo(t))
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			source, workcopies := createRecoveryCleanupSource(t), t.TempDir()
			previousCloneURL := repoCloneURL
			repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
			t.Cleanup(func() { repoCloneURL = previousCloneURL })
			manager, err := worktree.NewManager(workcopies)
			if err != nil {
				t.Fatal(err)
			}
			option, err := recoveryCleanupOption(layout, cfg, workcopies, repoCloneURL, journal.NewRegistryScrubber(), nil)
			if err != nil {
				t.Fatal(err)
			}
			option(manager)

			const gone, live = "cleanup-gone-checkout", "cleanup-live-checkout"
			gonePath := createRunningRecoveryWorkspace(t, layout, manager, source, gone)
			livePath := createRunningRecoveryWorkspace(t, layout, manager, source, live)
			preparation, err := recovery.PreparedRestoreBranch(gone)
			if err != nil {
				t.Fatal(err)
			}
			if tc.preparation {
				writeFixtureFile(t, gonePath, "prepared.txt", "restored work\n")
				runTestGit(t, gonePath, "add", "prepared.txt")
				runTestGit(t, gonePath, "-c", "user.name=Test User", "-c", "user.email=test@example.invalid", "commit", "-m", "prepared")
				runTestGit(t, gonePath, "branch", preparation, "HEAD")
			}
			if err := os.RemoveAll(gonePath); err != nil {
				t.Fatal(err)
			}

			results, warnings, err := manager.Reap(context.Background(), worktree.ReapOptions{
				IsRunAbandoned: func(_, owner string) (bool, error) { return owner == gone, nil },
			})
			if err != nil || len(warnings) != 0 {
				t.Fatalf("Reap() err=%v warnings=%v; want the gone checkout settled", err, warnings)
			}
			if len(results) != 1 || results[0].Path != gonePath {
				t.Fatalf("Reap() results = %+v, want only %s", results, gonePath)
			}
			if _, err := os.Stat(livePath); err != nil {
				t.Fatalf("live run's worktree was disturbed: %v", err)
			}
			if again, warnings, err := manager.Reap(context.Background(), worktree.ReapOptions{}); err != nil || len(again) != 0 || len(warnings) != 0 {
				t.Fatalf("second Reap() = %+v, %v, %v; want the entry retired", again, warnings, err)
			}

			assertPreparationBranchGone(t, manager, source, preparation)
			entries, err := os.ReadDir(filepath.Join(layout.Root, "recovery"))
			if err != nil {
				t.Fatal(err)
			}
			snapshots := 0
			for _, entry := range entries {
				if entry.IsDir() {
					snapshots++
				}
			}
			if want := map[bool]int{false: 0, true: 1}[tc.preparation]; snapshots != want {
				t.Fatalf("recovery inventory holds %d snapshots, want %d", snapshots, want)
			}
			assertMissingTargetRecorded(t, layout, gone, "shared")
		})
	}
}

// A historical marker (no durable base) whose checkout is gone has nothing to
// prove unchanged, so it is settled rather than quarantined for review.
func TestRecoveryCleanupHistoricalGoneCheckoutIsNotQuarantined(t *testing.T) {
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	workcopies := t.TempDir()
	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "cleanup-historical-gone"
	startedAt := time.Now().UTC().Add(-time.Hour)
	createFinishedRun(t, layout, runID, startedAt)
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}).CanonicalKey()
	target := worktree.CleanupTarget{
		Path: filepath.Join(workcopies, "repo-key", "runs", "wt-gone"), WorktreeID: runID + "-stage",
		OwnerRunID: runID, StartRef: "0123456789012345678901234567890123456789", CreatedAt: startedAt,
	}
	if err := recoveryCleanupHistoricalTarget(context.Background(), layout, cfg, workcopies, journal.NewRegistryScrubber(), manager, key, target); err != nil {
		t.Fatalf("historical cleanup of a gone checkout = %v, want settled", err)
	}
	assertMissingTargetRecorded(t, layout, runID, "unavailable")
}

func createRunningRecoveryWorkspace(t *testing.T, layout instance.Layout, manager *worktree.Manager, source, runID string) string {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Workflow: "implementation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: time.Now().UTC().Add(-time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: source, RunID: runID + "-query-backlog", OwnerRunID: runID,
		BaseRef: "main", Branch: "goobers/implementation/" + runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return workspace.Path
}

func assertPreparationBranchGone(t *testing.T, manager *worktree.Manager, source, branch string) {
	t.Helper()
	found, err := manager.WithExistingMirror(context.Background(), source, func(repository string) error {
		output, err := testgit.Command("-c", "safe.bareRepository=all", "-C", repository, "branch", "--list", branch).CombinedOutput()
		if err != nil {
			t.Fatalf("list preparation branch: %v: %s", err, output)
		}
		if strings.TrimSpace(string(output)) != "" {
			t.Fatalf("abandoned preparation branch %s survived cleanup", branch)
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("inspect shared repository: found=%t err=%v", found, err)
	}
}

func assertMissingTargetRecorded(t *testing.T, layout instance.Layout, runID, want string) {
	t.Helper()
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.RunID == runID && event.Runner["kind"] == recovery.GoneTargetEventKind {
			if event.Runner["preparationRepository"] != want {
				t.Fatalf("missing target recorded preparationRepository=%v, want %s", event.Runner["preparationRepository"], want)
			}
			return
		}
	}
	t.Fatalf("no missing-target record for %s", runID)
}
