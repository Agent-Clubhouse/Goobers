package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeriveStartupBudgetFromMeasuredAccumulation(t *testing.T) {
	accumulation := startupAccumulation{Worktrees: 4, RecoveryRuns: 6}
	got := deriveStartupBudget(2*time.Minute, accumulation)
	want := 2*time.Minute + 10*startupBudgetPerCandidate
	if got != want {
		t.Fatalf("deriveStartupBudget() = %s, want %s", got, want)
	}

	state, used := startupBudgetState(want*4/5, want)
	if state != "approaching" || used != 80 {
		t.Fatalf("startupBudgetState() = %q, %.1f; want approaching, 80", state, used)
	}
	state, _ = startupBudgetState(want, want)
	if state != "exceeded" {
		t.Fatalf("startupBudgetState() at budget = %q, want exceeded", state)
	}
}

func TestWorktreeAccumulationMeasuresMarkerAndMarkerlessUnion(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	for _, path := range []string{
		filepath.Join(repository, "markers"),
		filepath.Join(repository, "runs", "run-a"),
		filepath.Join(repository, "runs", "run-c"),
		filepath.Join(root, "scratch", "stage-a"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, runID := range []string{"run-a", "run-b"} {
		data := []byte(`{"run_id":"` + runID + `"}`)
		if err := os.WriteFile(filepath.Join(repository, "markers", runID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := worktreeAccumulationAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("worktreeAccumulationAt() = %d, want 3", got)
	}
}

func TestStartupBudgetSnapshotReportsMeasuredSignal(t *testing.T) {
	tracker := &startupPhaseTracker{}
	tracker.configureBudget(2 * time.Minute)
	tracker.setWorktreeAccumulation(3)
	tracker.setRecoveryAccumulation(2)
	now := time.Now()
	tracker.mu.Lock()
	tracker.budgetStarted = now.Add(-90 * time.Second)
	tracker.mu.Unlock()

	got := tracker.budgetSnapshot(now)
	if got.Accumulation.total() != 5 ||
		got.Budget != 2*time.Minute+5*startupBudgetPerCandidate ||
		got.State != "within-budget" {
		t.Fatalf("budget snapshot = %+v", got)
	}

	tracker.completeBudget(now)
	finished := tracker.budgetSnapshot(now.Add(time.Hour))
	if finished.Elapsed != got.Elapsed {
		t.Fatalf("completed budget elapsed = %s, want frozen %s", finished.Elapsed, got.Elapsed)
	}
}
