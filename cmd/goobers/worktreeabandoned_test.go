package main

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func seedRunPhase(t *testing.T, runsDir, runID string, phase journal.RunPhase) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "wf", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create run %s: %v", runID, err)
	}
	if phase != journal.PhaseRunning {
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
			t.Fatalf("finish run %s: %v", runID, err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run %s: %v", runID, err)
	}
}

// The abandoned sweep must fire for ordinary settled runs and must NOT fire
// for an escalated one. An escalated run is parked awaiting a remediation
// handoff whose need for the originating worktree is still an open question
// (#3098); reaping it would settle #3098 by destroying the evidence.
func TestWorktreeRunAbandonedSettledPhasesOnly(t *testing.T) {
	for _, tc := range []struct {
		phase journal.RunPhase
		want  bool
	}{
		{journal.PhaseCompleted, true},
		{journal.PhaseFailed, true},
		{journal.PhaseAborted, true},
		{journal.PhaseEscalated, false},
		{journal.PhaseRunning, false},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			runsDir := t.TempDir()
			const owner = "owner-run"
			seedRunPhase(t, runsDir, owner, tc.phase)
			got, err := worktreeRunAbandoned(runsDir)(owner+"-stage", owner)
			if err != nil {
				t.Fatalf("worktreeRunAbandoned: %v", err)
			}
			if got != tc.want {
				t.Fatalf("phase %s abandoned = %v, want %v", tc.phase, got, tc.want)
			}
		})
	}
}

// settledRunPhase is deliberately narrower than terminalRunPhase. Pin the
// difference so a later edit to either cannot silently merge them.
func TestSettledRunPhaseIsTerminalMinusEscalated(t *testing.T) {
	for _, phase := range []journal.RunPhase{
		journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted,
	} {
		if !settledRunPhase(phase) || !terminalRunPhase(phase) {
			t.Fatalf("%s must be both settled and terminal", phase)
		}
	}
	if settledRunPhase(journal.PhaseEscalated) {
		t.Fatal("escalated must not be settled")
	}
	if !terminalRunPhase(journal.PhaseEscalated) {
		t.Fatal("escalated must remain terminal — only the settled predicate excludes it")
	}
}

// A worktree ID that does not prefix-match its owner resolves only via the
// marker's stamped OwnerRunID. This is the difference between
// worktreeRunAbandoned and worktreeRunTerminal, so prove it rather than
// assume it.
func TestWorktreeRunAbandonedUsesStampedOwnerNotPrefix(t *testing.T) {
	runsDir := t.TempDir()
	seedRunPhase(t, runsDir, "unrelated-name", journal.PhaseCompleted)

	abandoned, err := worktreeRunAbandoned(runsDir)("wt-hash-with-no-shared-prefix", "unrelated-name")
	if err != nil {
		t.Fatalf("worktreeRunAbandoned: %v", err)
	}
	if !abandoned {
		t.Fatal("stamped owner run ID was not used to resolve the owning journal")
	}

	// With no stamped owner and no prefix match there is no journal to
	// authorize removal, so the answer must be a safe false.
	abandoned, err = worktreeRunAbandoned(runsDir)("wt-hash-with-no-shared-prefix", "")
	if err != nil {
		t.Fatalf("worktreeRunAbandoned (legacy marker): %v", err)
	}
	if abandoned {
		t.Fatal("unresolvable owner must not authorize removal")
	}
}
