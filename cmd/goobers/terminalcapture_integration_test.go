//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationTerminalRunBranchCapture pins the terminal-time capture that
// closes the nonterminal/terminal cleanup gap: every worktree is gone before
// the run turns terminal, so the run branch in the mirror is the only place
// the implementation still exists.
func TestIntegrationTerminalRunBranchCapture(t *testing.T) {
	testdep.Require(t, "git")
	for _, tc := range []struct {
		name string
		// commit advances the run branch past the base.
		commit bool
		// untracked leaves harness-shaped debris behind, which makes the
		// stage cleanup publish a snapshot of that same HEAD — so the
		// terminal capture must find the branch tip already covered.
		untracked  bool
		wantRecord bool
	}{
		{name: "branch-ahead-of-base", commit: true, wantRecord: true},
		{name: "already-covered-by-stage-capture", commit: true, untracked: true, wantRecord: true},
		{name: "branch-at-base", wantRecord: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workcopies, startedAt, records := runTerminalBranchCaptureFixture(t, tc.commit, tc.untracked)
			assertTerminalCaptureLeavesNoScratch(t, workcopies)
			want := 0
			if tc.wantRecord {
				want = 1
			}
			if len(records) != want {
				t.Fatalf("terminal finalization published %d recovery records, want %d", len(records), want)
			}
			if want == 0 {
				return
			}
			if records[0].RunID != terminalCaptureRunID {
				t.Fatalf("record belongs to run %q", records[0].RunID)
			}
			if records[0].BaseRef != "refs/heads/main" {
				t.Fatalf("record base ref = %q, want the run's base", records[0].BaseRef)
			}
			// Which writer produced the single record is what separates the
			// two one-record cases, and asserting only the count would let
			// either one pass for the other's reason. A stage capture is
			// stamped with the run's start; a terminal capture with its
			// durable finish event, which the fixture seeds strictly later.
			// No wall clock is consulted: both are the fixture's own values.
			stageCapture := records[0].CreatedAt.Equal(startedAt)
			if stageCapture != tc.untracked {
				t.Fatalf("record createdAt = %s (run started %s): stage capture = %t, want %t",
					records[0].CreatedAt, startedAt, stageCapture, tc.untracked)
			}
		})
	}
}

const terminalCaptureRunID = "terminal-branch-capture"

// runTerminalBranchCaptureFixture drives one run all the way to terminal with
// its stage worktree removed while the run was still nonterminal, and returns
// every recovery record the whole lifecycle published.
func runTerminalBranchCaptureFixture(t *testing.T, commit, untracked bool) (string, time.Time, []recovery.Record) {
	t.Helper()
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source, workcopies := t.TempDir(), t.TempDir()
	previousCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = previousCloneURL })
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")

	branch := "goobers/implementation/" + terminalCaptureRunID
	startedAt := time.Now().UTC().Add(-time.Hour)
	run := seedTerminalCaptureRun(t, layout, branch, startedAt)
	defer func() { _ = run.Close() }()

	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	option, err := recoveryCleanupOption(layout, cfg, workcopies, repoCloneURL, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	workspace, err := manager.Create(ctx, worktree.CreateOptions{
		RepoURL: source, RunID: terminalCaptureRunID + "-stage", OwnerRunID: terminalCaptureRunID,
		BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if commit {
		if err := os.WriteFile(filepath.Join(workspace.Path, "implementation.txt"), []byte("recover me"), 0o600); err != nil {
			t.Fatal(err)
		}
		recoveryCLIGit(t, workspace.Path, "add", "implementation.txt")
		recoveryCLIGit(t, workspace.Path, "commit", "-m", "Implement feature")
	}
	if untracked {
		if err := os.WriteFile(filepath.Join(workspace.Path, "claimed-item.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The nonterminal teardown every stage boundary performs. The run is not
	// terminal yet, so this guard cannot know it is the last one.
	if err := workspace.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	standalone, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeTerminalRunWithClaimRelease(layout, nil, standalone, terminalCaptureRunID,
		func(instance.Layout, *journal.InstanceLog, string) error { return nil }); err != nil {
		t.Fatalf("terminal finalization: %v", err)
	}
	return workcopies, startedAt, readTerminalCaptureRecords(t, layout)
}

// seedTerminalCaptureRun creates the run journal and records the run branch the
// way the runner does, since that annotation is what terminal capture reads.
func seedTerminalCaptureRun(t *testing.T, layout instance.Layout, branch string, startedAt time.Time) *journal.Run {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: terminalCaptureRunID, Workflow: "implementation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "branch", ID: branch},
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

func readTerminalCaptureRecords(t *testing.T, layout instance.Layout) []recovery.Record {
	t.Helper()
	root := filepath.Join(layout.Root, "recovery")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var records []recovery.Record
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		record, err := recovery.ReadRetainedRecord(filepath.Join(root, entry.Name(), recovery.RecordFileName))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

// assertTerminalCaptureLeavesNoScratch proves the throwaway checkout capture
// materializes is gone afterwards. A leaked one would make the next capture,
// reap or retention pass argue about a worktree nothing owns — and, worse,
// leave a second copy of the implementation outside the inventory.
func assertTerminalCaptureLeavesNoScratch(t *testing.T, workcopies string) {
	t.Helper()
	repos, err := os.ReadDir(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if !repo.IsDir() {
			continue
		}
		scratch := filepath.Join(workcopies, repo.Name(), terminalCaptureScratchDirectory)
		if _, err := os.Stat(scratch); !os.IsNotExist(err) {
			t.Fatalf("terminal capture left its scratch checkout at %s: %v", scratch, err)
		}
	}
}

// terminalCaptureScratchDirectory mirrors internal/worktree's own constant.
const terminalCaptureScratchDirectory = "terminal-capture"

// TestIntegrationTerminalReboundBranchCapture pins the same terminal capture
// for a run whose stages were rebound onto an EXISTING branch — the
// pr-remediation shape (#5399). The run's nominal branch is never advanced and
// carries nothing; the commits are on the pull request's branch, which only the
// runner's rebound-branch annotation names.
//
// A rebound branch is ahead of base before this run touches it, so the
// annotation's bound commit — not "ahead of base" — is what separates this
// run's work from the pull request's own.
func TestIntegrationTerminalReboundBranchCapture(t *testing.T) {
	testdep.Require(t, "git")
	for _, tc := range []struct {
		name string
		// aheadBeforeRun gives the pull request's branch a commit of its own
		// before this run ever binds to it.
		aheadBeforeRun bool
		// commit advances the rebound branch past the commit the run bound to.
		commit bool
		// untracked leaves harness-shaped debris behind, which makes the
		// stage cleanup publish a snapshot of that same HEAD — so the
		// terminal capture must find the rebound tip already covered.
		untracked  bool
		wantRecord bool
	}{
		{name: "rebound-branch-ahead-of-base", commit: true, wantRecord: true},
		{name: "rebound-tip-already-covered", commit: true, untracked: true, wantRecord: true},
		{name: "rebound-tip-at-base", wantRecord: false},
		// The run bound to a pull request that already had work, failed a
		// stage and committed nothing: there is nothing of this run's to
		// protect, and the pull request's own commits are not this run's to
		// retain.
		{name: "pull-request-work-only", aheadBeforeRun: true, wantRecord: false},
		{name: "commits-on-top-of-pull-request-work", aheadBeforeRun: true, commit: true, wantRecord: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := runTerminalReboundBranchCaptureFixture(t, terminalReboundOptions{
				aheadBeforeRun: tc.aheadBeforeRun, commit: tc.commit, untracked: tc.untracked,
			})
			assertTerminalCaptureLeavesNoScratch(t, fixture.workcopies)
			want := 0
			if tc.wantRecord {
				want = 1
			}
			if len(fixture.records) != want {
				t.Fatalf("terminal finalization published %d recovery records, want %d", len(fixture.records), want)
			}
			if want == 0 {
				return
			}
			record := fixture.records[0]
			if record.RunID != terminalReboundRunID {
				t.Fatalf("record belongs to run %q", record.RunID)
			}
			if record.BaseRef != "refs/heads/main" {
				t.Fatalf("record base ref = %q, want the run's base", record.BaseRef)
			}
			assertRecordCoversCommit(t, fixture.mirror, record, fixture.reboundTip)
			// A stage capture is stamped with the run's start, a terminal
			// capture with the run's durable finish event, which the fixture
			// seeds strictly later. No wall clock is consulted: both are the
			// fixture's own values. The covered case must therefore be the
			// stage capture's single record and no second one.
			stageCapture := record.CreatedAt.Equal(fixture.startedAt)
			if stageCapture != tc.untracked {
				t.Fatalf("record createdAt = %s (run started %s): stage capture = %t, want %t",
					record.CreatedAt, fixture.startedAt, stageCapture, tc.untracked)
			}
		})
	}
}

const (
	terminalReboundRunID = "terminal-rebound-capture"
	// terminalReboundBranch stands in for the pull request's head branch a
	// remediation run is rebound onto: it belongs to another run, and this run
	// never advances the nominal branch it was started with.
	terminalReboundBranch = "goobers/implementation/terminal-rebound-subject"
)

// terminalReboundOptions shapes one rebound-branch lifecycle.
type terminalReboundOptions struct {
	aheadBeforeRun bool
	commit         bool
	untracked      bool
}

// terminalReboundFixture is one rebound-branch lifecycle's observable result.
type terminalReboundFixture struct {
	workcopies string
	mirror     string
	reboundTip string
	startedAt  time.Time
	records    []recovery.Record
}

// runTerminalReboundBranchCaptureFixture drives one run that binds its writable
// workspace to an already existing branch, optionally commits there, loses its
// worktree while still nonterminal and then fails — the observed shape of a
// remediation run whose push was rejected.
func runTerminalReboundBranchCaptureFixture(t *testing.T, opts terminalReboundOptions) terminalReboundFixture {
	t.Helper()
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source, workcopies := t.TempDir(), t.TempDir()
	previousCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = previousCloneURL })
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	seedTerminalReboundPullRequestBranch(t, source, opts.aheadBeforeRun)

	startedAt := time.Now().UTC().Add(-time.Hour)
	run := seedTerminalReboundRun(t, layout, startedAt)
	defer func() { _ = run.Close() }()

	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	option, err := recoveryCleanupOption(layout, cfg, workcopies, repoCloneURL, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	workspace, err := manager.Create(ctx, worktree.CreateOptions{
		RepoURL: source, RunID: terminalReboundRunID + "-stage", OwnerRunID: terminalReboundRunID,
		BaseRef: "main", Branch: terminalReboundBranch,
		// The rebinding the runner applies for an existing branch.
		RequireExistingBranch: true, AcquireRemoteBranch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The runner records the rebound branch once its worktree exists, stamped
	// with the commit that worktree was checked out at.
	appendTerminalReboundAnnotation(t, run, recoveryCLIGit(t, workspace.Path, "rev-parse", "HEAD"))
	if opts.commit {
		if err := os.WriteFile(filepath.Join(workspace.Path, "remediation.txt"), []byte("recover me"), 0o600); err != nil {
			t.Fatal(err)
		}
		recoveryCLIGit(t, workspace.Path, "add", "remediation.txt")
		recoveryCLIGit(t, workspace.Path, "commit", "-m", "Remediate pull request")
	}
	if opts.untracked {
		if err := os.WriteFile(filepath.Join(workspace.Path, "claimed-item.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reboundTip := recoveryCLIGit(t, workspace.Path, "rev-parse", "HEAD")
	// The nonterminal teardown every stage boundary performs, then the push
	// failure that ends the run.
	if err := workspace.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	standalone, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeTerminalRunWithClaimRelease(layout, nil, standalone, terminalReboundRunID,
		func(instance.Layout, *journal.InstanceLog, string) error { return nil }); err != nil {
		t.Fatalf("terminal finalization: %v", err)
	}
	return terminalReboundFixture{
		workcopies: workcopies,
		mirror:     filepath.Join(workcopies, worktree.RepositoryKey(source), "repo.git"),
		reboundTip: reboundTip,
		startedAt:  startedAt,
		records:    readTerminalCaptureRecords(t, layout),
	}
}

// seedTerminalReboundPullRequestBranch creates the branch the run is rebound
// onto, with a commit of the pull request's own when ahead is set.
func seedTerminalReboundPullRequestBranch(t *testing.T, source string, ahead bool) {
	t.Helper()
	recoveryCLIGit(t, source, "branch", terminalReboundBranch, "main")
	if !ahead {
		return
	}
	recoveryCLIGit(t, source, "checkout", terminalReboundBranch)
	// Real content, not an empty commit: a capture of an empty cumulative
	// diff is dropped by SkipEmpty, which would make this case pass for a
	// reason that has nothing to do with the bound commit.
	if err := os.WriteFile(filepath.Join(source, "pull-request-work.txt"), []byte("existing work"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, source, "add", "pull-request-work.txt")
	recoveryCLIGit(t, source, "commit", "-m", "the pull request's own work")
	recoveryCLIGit(t, source, "checkout", "main")
}

// seedTerminalReboundRun records the run's own nominal branch exactly as the
// runner does. The nominal branch is never advanced, which is precisely why
// reading it alone protected nothing.
func seedTerminalReboundRun(t *testing.T, layout instance.Layout, startedAt time.Time) *journal.Run {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: terminalReboundRunID, Workflow: "pr-remediation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{
			Provider: "github", Kind: "branch",
			ID: "goobers/pr-remediation/" + terminalReboundRunID,
		},
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

// appendTerminalReboundAnnotation writes the annotation a stage provisioned on
// an existing branch leaves behind.
func appendTerminalReboundAnnotation(t *testing.T, run *journal.Run, boundSHA string) {
	t.Helper()
	if err := run.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"annotation":                    runner.ReboundWorkspaceBranchAnnotation,
			runner.WorkspaceBranchOutput:    terminalReboundBranch,
			runner.ReboundBranchBoundSHAKey: boundSHA,
			"repository":                    "github/example/example",
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// assertRecordCoversCommit proves the published record protects the rebound
// branch tip rather than some other commit: the nominal branch, which the run
// never advanced, could not have produced it.
func assertRecordCoversCommit(t *testing.T, mirror string, record recovery.Record, tip string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	covered, err := recovery.SnapshotCoversCommit(ctx, mirror, record.SnapshotSHA, tip)
	if err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatalf("record snapshot %s does not cover the rebound branch tip %s", record.SnapshotSHA, tip)
	}
}
