package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// TestFinishRecordsTerminalEvenWhenRecordPinnedOutcomeFails is the
// recordPinnedOutcome half of the stranded-claim fix
// (TestFinishRecordsTerminalEvenWhenPrepareTerminalFails, in
// terminal_durable_test.go, is the PrepareTerminal half).
//
// PinnedLease.RecordOutcome does its own file I/O against the pinned
// workspace's failure-streak record, independently of terminal state
// persistence. Before the fix, finishTakeover returned immediately on that
// error, skipping prepareTerminal AND the run.finished append entirely — the
// run reconstructs as PhaseRunning forever and the claim is stranded.
//
// The contract asserted here mirrors the PrepareTerminal test: the run is
// durably terminal, and the RecordOutcome error is still surfaced to the
// caller rather than swallowed.
func TestFinishRecordsTerminalEvenWhenRecordPinnedOutcomeFails(t *testing.T) {
	repo := newFixtureRepo(t)
	instanceRoot := t.TempDir()
	workcopies := filepath.Join(instanceRoot, "workcopies")
	wtMgr, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")

	byTask := map[string]stubTaskResult{
		"pinned-outcome-1:implement": {status: apiv1.ResultSuccess},
		"pinned-outcome-2:implement": {status: apiv1.ResultSuccess},
	}
	newDet := func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: byTask}, nil
	}
	r, err := New(Config{
		PinnedWorkspace:  true,
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return repo, nil },
		Automated:        &envelopeCapturingAutomated{},
		NewDeterministic: newDet,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	machine := fixtureMachine(t)
	ref := humanGateRepoRef()

	start := func(runID string) (Result, error) {
		return r.Start(context.Background(), StartInput{
			RunID: runID, Machine: machine, Gaggle: "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: ref,
		})
	}

	// The first run completes normally, creating the pinned workspace's
	// failure-streak record (root/failure-streak.json) as a side effect of its
	// own (successful) recordPinnedOutcome call, and releasing the pinned
	// lease so the second run below can reacquire it against the same repo.
	first, err := start("pinned-outcome-1")
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if first.Phase != journal.PhaseCompleted {
		t.Fatalf("first Start phase = %q, want completed", first.Phase)
	}

	streakPath := findFailureStreakFile(t, workcopies)
	if err := os.Remove(streakPath); err != nil {
		t.Fatalf("remove failure-streak.json: %v", err)
	}
	// Replacing the file with a directory of the same name makes the second
	// run's RecordOutcome fail deterministically (os.ReadFile on a directory
	// errors on every platform, unlike a chmod-based failure, which root or
	// Windows can silently ignore).
	if err := os.Mkdir(streakPath, 0o755); err != nil {
		t.Fatalf("mkdir in place of failure-streak.json: %v", err)
	}

	second, err := start("pinned-outcome-2")
	if err == nil {
		t.Fatal("expected the RecordOutcome error to be surfaced, not swallowed")
	}
	if second.Phase != journal.PhaseCompleted {
		t.Fatalf("second Start result phase = %q, want completed despite the RecordOutcome failure", second.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "pinned-outcome-2"))
	if err != nil {
		t.Fatal(err)
	}
	// The decisive assertion: the run is terminal on disk. Before the fix this
	// reconstructed PhaseRunning forever and the claim stayed held.
	got, err := rd.Phase()
	if err != nil {
		t.Fatal(err)
	}
	if got != journal.PhaseCompleted {
		t.Fatalf("reconstructed phase = %q, want completed -- a failed RecordOutcome must not keep a run running", got)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var sawFinished bool
	for _, ev := range events {
		if ev.Type == journal.EventRunFinished {
			sawFinished = true
		}
	}
	if !sawFinished {
		t.Fatal("run.finished must be appended even when RecordOutcome failed")
	}
}

// findFailureStreakFile locates the pinned workspace's failure-streak.json
// under root, without depending on worktree's unexported repo-key derivation.
func findFailureStreakFile(t *testing.T, root string) string {
	t.Helper()
	var found string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == "failure-streak.json" {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if found == "" {
		t.Fatalf("no failure-streak.json found under %s", root)
	}
	return found
}
