package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func TestPruneConfiguredRetentionDefaultsOffThenDryRunsAndDeletes(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	const runID = "retention-failure"
	createTerminalRun(t, layout, runID)
	manager, repo := commandWorktreeFixture(t, layout)
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: runID + "-stage", OwnerRunID: runID, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := wt.Remove(context.Background(), worktree.RemoveOptions{Keep: true}); err != nil {
		t.Fatalf("keep worktree: %v", err)
	}
	setup := &schedulerSetup{
		Config:          &instance.Config{},
		LegacyWorktrees: manager,
	}

	var stdout, stderr bytes.Buffer
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("disabled prune: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("disabled retention output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("default retention removed worktree: %v", err)
	}

	setup.Config.Retention = instance.RetentionConfig{DryRun: true, MaxRetainedWorktreeBytes: 1}
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("dry-run prune: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "retention candidate rule=storage-cap kind=worktree") {
		t.Fatalf("dry-run output = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("dry-run stderr = %q", stderr.String())
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("dry-run removed worktree: %v", err)
	}

	stdout.Reset()
	setup.Config.Retention = instance.RetentionConfig{Enabled: true, MaxRetainedWorktreeBytes: 1}
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("enabled prune: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "retention deleted rule=storage-cap kind=worktree") ||
		!strings.Contains(got, "reclaimedBytes=") {
		t.Fatalf("delete output = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("delete stderr = %q", stderr.String())
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("enabled retention left worktree: %v", err)
	}
}

// TestPruneConfiguredRetentionReclaimsJournalLessWorktreeAfterGraceWindow is
// #2052's fix for a retained worktree whose owning run journal is deleted
// out from under it (e.g. by telemetry retention, which prunes
// independently and on its own schedule): IsTerminalFailure can never
// authorize such a worktree again since there's nothing left to read, so
// without the grace-window rule it was permanently unprunable. Proves both
// halves: left alone while inside the grace window, reclaimed once past it.
func TestPruneConfiguredRetentionReclaimsJournalLessWorktreeAfterGraceWindow(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	manager, repo := commandWorktreeFixture(t, layout)
	setup := &schedulerSetup{
		Config:          &instance.Config{Retention: instance.RetentionConfig{Enabled: true}},
		LegacyWorktrees: manager,
	}

	makeJournalLessRetainedWorktree := func(runID string) *worktree.Worktree {
		t.Helper()
		createTerminalRun(t, layout, runID)
		wt, err := manager.Create(context.Background(), worktree.CreateOptions{
			RepoURL: repo, RunID: runID + "-stage", OwnerRunID: runID, BaseRef: "main",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := wt.Remove(context.Background(), worktree.RemoveOptions{Keep: true}); err != nil {
			t.Fatalf("keep worktree: %v", err)
		}
		// Simulate telemetry retention deleting the run's journal directory
		// independently of worktree retention (#2052's grounding scenario).
		if err := os.RemoveAll(filepath.Join(layout.RunsDir(), runID)); err != nil {
			t.Fatalf("remove run journal for %s: %v", runID, err)
		}
		return wt
	}

	prevGraceAge := journalGraceAge
	t.Cleanup(func() { journalGraceAge = prevGraceAge })

	// Still inside a real 24h grace window: left in place.
	journalGraceAge = prevGraceAge
	insideWindow := makeJournalLessRetainedWorktree("journal-less-fresh")
	var stdout, stderr bytes.Buffer
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("prune inside grace window: %v", err)
	}
	if _, err := os.Stat(insideWindow.Path); err != nil {
		t.Fatalf("journal-grace reclaimed a worktree still inside its grace window: %v", err)
	}

	// Shrunk to effectively zero (any positive value): the fixture above was
	// already created before this point, so its real retainedAt is already
	// in the past relative to a nanosecond-scale window.
	journalGraceAge = time.Nanosecond
	stdout.Reset()
	stderr.Reset()
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("prune past grace window: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "retention deleted rule=journal-grace kind=worktree") {
		t.Fatalf("past-grace-window output = %q, want a journal-grace deletion", got)
	}
	if _, err := os.Stat(insideWindow.Path); !os.IsNotExist(err) {
		t.Fatalf("journal-grace left a worktree past its (shrunk) grace window in place: %v", err)
	}
}

func TestPruneConfiguredRetentionProtectsPausedRunReboundBranchOnRestart(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	manager, repo := commandWorktreeFixture(t, layout)
	ctx := context.Background()

	createMergedTerminalBranch := func(runID string) string {
		t.Helper()
		branch := providers.BranchName("implementation", runID)
		wt, err := manager.Create(ctx, worktree.CreateOptions{
			RepoURL: repo, RunID: runID + "-stage", OwnerRunID: runID,
			BaseRef: "main", Branch: branch,
		})
		if err != nil {
			t.Fatalf("Create branch %s: %v", branch, err)
		}
		if err := wt.Remove(ctx, worktree.RemoveOptions{}); err != nil {
			t.Fatalf("remove branch fixture worktree: %v", err)
		}
		createTerminalRun(t, layout, runID)
		return branch
	}

	protectedBranch := createMergedTerminalBranch("terminal-owner")
	eligibleBranch := createMergedTerminalBranch("eligible-owner")
	repoDir, err := manager.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy: %v", err)
	}
	if !retentionBranchExists(repoDir, protectedBranch) || !retentionBranchExists(repoDir, eligibleBranch) {
		t.Fatalf("merged branch fixtures missing before retention")
	}

	machine, err := workflow.Compile(workflow.Definition{
		Name: "pr-remediation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example",
			Start:  "select-old-branch",
			Tasks: []apiv1.Task{
				{
					Name: "select-old-branch", Type: apiv1.TaskDeterministic,
					Goal: "select the prior branch",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
					Next: "gather-pr-context",
				},
				{
					Name: "gather-pr-context", Type: apiv1.TaskDeterministic,
					Goal: "select the current PR branch",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
					Next: workflow.TerminalComplete,
				},
			},
		},
	}, workflow.WithPreviewFeatures(true))

	if err != nil {
		t.Fatalf("compile fixture workflow: %v", err)
	}
	const pausedRunID = "paused-remediation"
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: pausedRunID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create paused run: %v", err)
	}
	for _, binding := range []struct {
		stage  string
		branch string
	}{
		{stage: "select-old-branch", branch: eligibleBranch},
		{stage: "gather-pr-context", branch: protectedBranch},
	} {
		if err := run.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: binding.stage, Status: string(apiv1.ResultSuccess),
			Outputs: map[string]any{runner.WorkspaceBranchOutput: binding.branch},
		}); err != nil {
			t.Fatalf("append %s workspace rebinding: %v", binding.stage, err)
		}
	}
	if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: "review"}); err != nil {
		t.Fatalf("append paused gate: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close paused run: %v", err)
	}

	setup := &schedulerSetup{
		Config:          &instance.Config{Retention: instance.RetentionConfig{Enabled: true}},
		LegacyWorktrees: manager,
		Machines: map[localscheduler.WorkflowIdentity]*workflow.Machine{
			{Gaggle: "example", Workflow: machine.Def.Name}: machine,
		},
	}
	protected, err := retentionProtectedBranches(map[string]string{manager.Root: layout.RunsDir()}, setup)
	if err != nil {
		t.Fatalf("collect protected branches: %v", err)
	}
	if _, ok := protected[manager.Root][protectedBranch]; !ok {
		t.Fatalf("paused run branch %q was not protected: %+v", protectedBranch, protected)
	}
	if _, ok := protected[manager.Root][eligibleBranch]; ok {
		t.Fatalf("superseded rebound branch %q remained protected: %+v", eligibleBranch, protected)
	}
	var stdout, stderr bytes.Buffer
	if err := pruneConfiguredRetention(ctx, layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("prune on restart: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("retention stderr = %q", stderr.String())
	}
	if !retentionBranchExists(repoDir, protectedBranch) {
		t.Fatalf("retention deleted paused run rebound branch %q", protectedBranch)
	}
	if retentionBranchExists(repoDir, eligibleBranch) {
		t.Fatalf("retention left unrelated eligible branch %q", eligibleBranch)
	}
}

func retentionBranchExists(repoDir, branch string) bool {
	cmd := exec.Command("git", "-c", "safe.bareRepository=all", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}
