package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// TestPruneConfiguredRetentionDefaultsOnAndHoldsAGraceWindow pins #4253's
// flip end to end on a stock instance — one whose instance.yaml says nothing
// at all about retention.
//
// It replaces TestPruneConfiguredRetentionDefaultsOffThenDryRunsAndDeletes,
// whose first phase asserted the opposite policy (a stock config prunes
// nothing). That assertion was rewritten because the policy changed, not
// nudged until it passed.
//
// The four phases are the whole contract: a stock instance with nothing old
// enough does nothing and starts no clock; once something crosses the default
// age bound it is reported and the grace clock starts; once that clock
// elapses the same candidate is actually deleted; and an explicit opt-out
// stops the pass before any of it.
func TestPruneConfiguredRetentionDefaultsOnAndHoldsAGraceWindow(t *testing.T) {
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
	// A stock instance: no retention block at all.
	setup := &schedulerSetup{
		Config:          &instance.Config{},
		LegacyWorktrees: manager,
	}

	start := time.Now()
	clock := start
	restore := retentionNow
	retentionNow = func() time.Time { return clock }
	t.Cleanup(func() { retentionNow = restore })

	var stdout, stderr bytes.Buffer
	prune := func(phase string) string {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
			t.Fatalf("%s prune: %v", phase, err)
		}
		if stderr.Len() != 0 {
			t.Fatalf("%s stderr = %q", phase, stderr.String())
		}
		return stdout.String()
	}

	// 1. The pass runs on a stock config, but the worktree is newer than the
	//    default age bound, so there is nothing to report and no clock to
	//    start. This is the case that must not become chatty: almost every
	//    instance is here almost all of the time.
	if got := prune("fresh"); got != "" {
		t.Fatalf("stock retention acted on a worktree inside the age bound: %q", got)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("fresh pass removed worktree: %v", err)
	}
	if state, ok, err := readRetentionGraceState(layout, worktreeRetentionStateFile); err != nil || !ok {
		t.Fatalf("fresh pass recorded no state: ok=%v err=%v", ok, err)
	} else if !state.EnforceAt.IsZero() {
		t.Fatalf("fresh pass started a grace window with no candidates: %+v", state)
	}

	// 2. Past the default bound the worktree becomes a candidate. The first
	//    such pass reports it and starts the grace clock — it must not delete.
	clock = start.Add(instance.DefaultRetainedWorktreeMaxAge + time.Hour)
	got := prune("aged")
	if !strings.Contains(got, "retention candidate rule=max-age kind=worktree") &&
		!strings.Contains(got, "retention candidate") {
		t.Fatalf("aged pass reported no candidate: %q", got)
	}
	if !strings.Contains(got, "first-enable grace window") {
		t.Fatalf("aged pass did not announce its grace window: %q", got)
	}
	if strings.Contains(got, "retention deleted") {
		t.Fatalf("aged pass deleted inside the grace window: %q", got)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("grace-window pass removed worktree: %v", err)
	}
	state, _, err := readRetentionGraceState(layout, worktreeRetentionStateFile)
	if err != nil {
		t.Fatalf("read grace state: %v", err)
	}
	if state.EnforceAt.IsZero() || !state.LastPassDryRun || state.CandidateCount == 0 {
		t.Fatalf("grace window not started: %+v", state)
	}

	// 3. A pass inside the window still only reports.
	clock = state.EnforceAt.Add(-time.Minute)
	if got := prune("inside window"); strings.Contains(got, "retention deleted") {
		t.Fatalf("pass inside the grace window deleted: %q", got)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("pass inside the window removed worktree: %v", err)
	}

	// 4. Once the window elapses, the same candidate is actually reclaimed.
	clock = state.EnforceAt.Add(time.Minute)
	if got := prune("enforcing"); !strings.Contains(got, "retention deleted") {
		t.Fatalf("pass after the grace window did not delete: %q", got)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("pass after the grace window left worktree: %v", err)
	}
}

func TestPruneConfiguredRetentionPersistsConservativeClockRepair(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	state := retentionGraceState{
		DetectedAt: now.Add(time.Hour),
		EnforceAt:  now.Add(time.Hour).Add(retentionGraceWindow),
	}
	if err := writeRetentionGraceState(layout, worktreeRetentionStateFile, worktreeRetentionStateSchema, state); err != nil {
		t.Fatal(err)
	}
	restore := retentionNow
	retentionNow = func() time.Time { return now }
	t.Cleanup(func() { retentionNow = restore })

	var stdout, stderr bytes.Buffer
	setup := &schedulerSetup{Config: &instance.Config{}}
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "corrected invalid worktree retention grace state") {
		t.Fatalf("repair emitted no operator warning: %q", stderr.String())
	}
	got, ok, err := readRetentionGraceState(layout, worktreeRetentionStateFile)
	if err != nil || !ok {
		t.Fatalf("read repaired state: ok=%v err=%v", ok, err)
	}
	if !got.DetectedAt.Equal(now) || !got.EnforceAt.Equal(now.Add(retentionGraceWindow)) {
		t.Fatalf("persisted repair = (%s, %s), want fresh window from %s", got.DetectedAt, got.EnforceAt, now)
	}
}

// TestPruneConfiguredRetentionExplicitOptOutDoesNothing keeps the escape hatch
// honest: #4253 flipped the default, it did not remove the ability to say no.
func TestPruneConfiguredRetentionExplicitOptOutDoesNothing(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	const runID = "retention-optout"
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
		Config:          &instance.Config{Retention: instance.RetentionConfig{Enabled: boolPtr(false)}},
		LegacyWorktrees: manager,
	}

	restore := retentionNow
	// Far past every bound: an opted-out instance must still do nothing.
	retentionNow = func() time.Time { return time.Now().Add(100 * 24 * time.Hour) }
	t.Cleanup(func() { retentionNow = restore })

	var stdout, stderr bytes.Buffer
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("opted-out prune: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("opted-out retention produced output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("opted-out retention removed worktree: %v", err)
	}
	if _, ok, err := readRetentionGraceState(layout, worktreeRetentionStateFile); err != nil || ok {
		t.Fatalf("opted-out retention wrote grace state: ok=%v err=%v", ok, err)
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
		Config: &instance.Config{Retention: instance.RetentionConfig{
			Enabled: boolPtr(true), FirstEnable: "immediate", JournalGraceAge: "24h",
		}},
		LegacyWorktrees: manager,
	}
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	previousNow := retentionNow
	retentionNow = func() time.Time { return now }
	t.Cleanup(func() { retentionNow = previousNow })

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

	// The first pass starts a real 24h grace window and leaves it in place.
	insideWindow := makeJournalLessRetainedWorktree("journal-less-fresh")
	var stdout, stderr bytes.Buffer
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("prune inside grace window: %v", err)
	}
	if _, err := os.Stat(insideWindow.Path); err != nil {
		t.Fatalf("journal-grace reclaimed a worktree still inside its grace window: %v", err)
	}

	// A later pass uses the persisted observation, including after restart.
	now = now.Add(24 * time.Hour)
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
		Config:          &instance.Config{Retention: instance.RetentionConfig{Enabled: boolPtr(true), FirstEnable: "immediate"}},
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

func TestRetentionProtectedBranchesRejectsFutureJournalSchema(t *testing.T) {
	runsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(runsDir, "00-unrelated"), 0o755); err != nil {
		t.Fatal(err)
	}
	futureDir := filepath.Join(runsDir, "01-future")
	if err := os.Mkdir(futureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(journal.SchemaInfo{
		Version:       journal.CurrentSchemaVersion + 1,
		MinimumBinary: "v2.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(futureDir, "schema.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = retentionProtectedBranches(map[string]string{"worktrees": runsDir}, &schedulerSetup{})
	if err == nil {
		t.Fatal("retention accepted a future journal schema")
	}
	for _, want := range []string{"01-future", "version 2", "supported version 1", "minimum binary is v2.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("retention error %q does not contain %q", err, want)
		}
	}
}

func retentionBranchExists(repoDir, branch string) bool {
	cmd := testgit.Command("-c", "safe.bareRepository=all", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}

// TestSweepWorktreeRetentionPrunesConfiguredRetention is #2052's ticker
// acceptance: pruneConfiguredRetention previously ran only in
// runUpContext's synchronous startup block, so a worktree kept for a
// failure that happened after startup sat on disk until the daemon's next
// restart. sweepWorktreeRetention is the exact function the new periodic
// ticker (worktreeRetentionSweepInterval) invokes on every tick; this
// proves a single call prunes a retention-eligible worktree, using the
// daemon's own setup.LegacyWorktrees manager for both creation and
// sweeping — a live, separately-timed ticker racing a second manager
// instance pointed at the same directory would instead race Reap's
// markerless-cleanup against an in-flight `git worktree add` (see git
// history for this test), which calling the sweep function directly avoids
// entirely.
//
// Manager.Reap's own crash-orphan behavior is exercised independently by
// internal/worktree's and cmd/goobers/worktreelifecycle_test.go's existing
// coverage; this test's job is only to prove the new periodic entry point
// wires pruneConfiguredRetention correctly.
func TestSweepWorktreeRetentionPrunesConfiguredRetention(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	ctx := context.Background()
	manager, repo := commandWorktreeFixture(t, layout)
	setup := &schedulerSetup{
		Config:          &instance.Config{Retention: instance.RetentionConfig{Enabled: boolPtr(true), MaxRetainedWorktreeBytes: 1, FirstEnable: "immediate"}},
		LegacyWorktrees: manager,
	}

	const retainedRunID = "retained-failure"
	createTerminalRun(t, layout, retainedRunID)
	retained, err := manager.Create(ctx, worktree.CreateOptions{
		RepoURL: repo, RunID: retainedRunID + "-stage", OwnerRunID: retainedRunID, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("create retained worktree: %v", err)
	}
	if err := retained.Remove(ctx, worktree.RemoveOptions{Keep: true}); err != nil {
		t.Fatalf("keep retained worktree: %v", err)
	}

	if err := sweepWorktreeRetention(ctx, layout, setup); err != nil {
		t.Fatalf("sweepWorktreeRetention: %v", err)
	}

	if _, err := os.Stat(retained.Path); !os.IsNotExist(err) {
		t.Fatalf("sweepWorktreeRetention did not prune the retained worktree: stat err = %v", err)
	}
}

// boolPtr builds the tri-state RetentionConfig.Enabled / TelemetryRetentionConfig.Enabled
// values, where nil means "unset, take the opt-out default" (#4253).
func boolPtr(v bool) *bool { return &v }

// TestReportWorktreeRetentionPolicySurfacesTheGraceWindow covers #4253's
// operator-visibility half. The flip makes most instances run a retention
// policy nobody configured; if the grace window is invisible, an operator has
// no chance to object before it starts deleting.
func TestReportWorktreeRetentionPolicySurfacesTheGraceWindow(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	now := time.Now()

	// Nothing recorded yet: silent, like the telemetry surface.
	var stdout bytes.Buffer
	reportWorktreeRetentionPolicy(layout, now, &stdout)
	if stdout.Len() != 0 {
		t.Fatalf("reported with no state: %q", stdout.String())
	}

	detectedAt := now.Add(-time.Hour)
	enforceAt := detectedAt.Add(retentionGraceWindow)
	write := func(state retentionGraceState) {
		t.Helper()
		if err := writeRetentionGraceState(layout, worktreeRetentionStateFile, worktreeRetentionStateSchema, state); err != nil {
			t.Fatalf("write state: %v", err)
		}
	}

	write(retentionGraceState{DetectedAt: detectedAt, LastPassAt: now.Add(-time.Minute), LastPassDryRun: true, EnforceAt: enforceAt, CandidateCount: 3})
	stdout.Reset()
	reportWorktreeRetentionPolicy(layout, now, &stdout)
	got := stdout.String()
	for _, want := range []string{"grace period active until", enforceAt.UTC().Format(time.RFC3339), "3 candidate(s)", "nothing deleted yet", "instance.yaml retention changes require a daemon restart", "materialized config directory"} {
		if !strings.Contains(got, want) {
			t.Errorf("grace-window line missing %q: %q", want, got)
		}
	}

	write(retentionGraceState{LastPassAt: now.Add(-time.Minute), LastPassDryRun: true})
	stdout.Reset()
	reportWorktreeRetentionPolicy(layout, now, &stdout)
	if got := stdout.String(); !strings.Contains(got, "policy in force, no candidates") {
		t.Errorf("quiet line = %q", got)
	}

	write(retentionGraceState{LastPassAt: now.Add(-time.Minute), PrunedCount: 7})
	stdout.Reset()
	reportWorktreeRetentionPolicy(layout, now, &stdout)
	if got := stdout.String(); !strings.Contains(got, "pruned 7 item(s)") {
		t.Errorf("enforcing line = %q", got)
	}
}
