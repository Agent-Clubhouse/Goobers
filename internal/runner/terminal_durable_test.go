package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestFinishRecordsTerminalWhenPinnedOutcomeWriteFails(t *testing.T) {
	r, in := readOnlyWorkspaceRunner(t)
	runID := "terminal-pinned-outcome-failure"
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := r.cfg.Worktrees.AcquirePinned(context.Background(), worktree.PinnedOptions{
		RepoURL: repoURL, RunID: runID, BaseRef: "main", Branch: "goobers/test/" + runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	r.pinnedRuns[runID] = lease

	roots, err := filepath.Glob(filepath.Join(r.cfg.Worktrees.PinnedRoot(), "*"))
	if err != nil || len(roots) != 1 {
		t.Fatalf("pinned roots = %v, %v; want one", roots, err)
	}
	if err := os.WriteFile(filepath.Join(roots[0], "failure-streak.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()
	finalized := false
	r.cfg.FinalizeTerminal = func(string, journal.RunPhase) error {
		finalized = true
		return nil
	}

	res, finishErr := r.finish(runID, jr, journal.PhaseFailed, "verify", 2)
	if finishErr == nil || !strings.Contains(finishErr.Error(), "decode pinned failure streak") {
		t.Fatalf("finish error = %v, want pinned outcome error", finishErr)
	}
	if res.Phase != journal.PhaseFailed || !finalized {
		t.Fatalf("result = %+v, finalized = %v; terminalization must still complete", res, finalized)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	if phase, err := reader.Phase(); err != nil || phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, %v; want failed", phase, err)
	}
}

// TestFinishRecordsTerminalEvenWhenPrepareTerminalFails is the runner half of
// the stranded-claim fix.
//
// PrepareTerminal performs external forge cleanup (terminal branch delete and
// run-aborted labeling), so it can fail independently of terminal state
// persistence. A cleanup error must not prevent the run.finished append or
// claim recovery.
//
// The contract asserted here: the run is durably terminal, the preparer's own
// failure fact survives in the journal, and the error is still surfaced to the
// caller rather than swallowed.
func TestFinishRecordsTerminalEvenWhenPrepareTerminalFails(t *testing.T) {
	for _, phase := range []journal.RunPhase{journal.PhaseAborted, journal.PhaseEscalated} {
		t.Run(string(phase), func(t *testing.T) {
			runID := "terminal-preparer-failure-" + string(phase)
			runsDir := t.TempDir()
			jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = jr.Close() }()

			prepareErr := errors.New("label run-aborted: 401 Bad credentials")
			var finalized bool
			r := &Runner{cfg: Config{
				PrepareTerminal: func(_ string, _ journal.RunPhase, gotJournal *journal.Run) error {
					// A real preparer journals its own failure fact before
					// returning the error; that diagnostic must survive.
					if err := gotJournal.Append(journal.Event{
						Type:        journal.EventRefTouched,
						ExternalRef: &journal.ExternalRef{Provider: "gitea", Kind: "pr", ID: "77"},
						Runner:      map[string]any{"operation": "label-run-aborted"},
						Error:       &journal.ErrorDetail{Code: "run_abort_label_failed", Message: prepareErr.Error()},
					}); err != nil {
						t.Fatal(err)
					}
					return prepareErr
				},
				FinalizeTerminal: func(string, journal.RunPhase) error {
					finalized = true
					return nil
				},
			}}

			res, err := r.finish(runID, jr, phase, "merged-gate", 3)
			if err == nil {
				t.Fatal("expected the preparer error to be surfaced, not swallowed")
			}
			if !strings.Contains(err.Error(), "401 Bad credentials") {
				t.Fatalf("error = %v, want the preparer failure", err)
			}
			if res.Phase != phase {
				t.Fatalf("result phase = %q, want %q", res.Phase, phase)
			}
			if !finalized {
				t.Fatal("FinalizeTerminal must still run so claims are released")
			}

			rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
			if err != nil {
				t.Fatal(err)
			}
			// The decisive assertion: the run is terminal on disk. Before the
			// fix this read PhaseRunning and the claim stayed held.
			got, err := rd.Phase()
			if err != nil {
				t.Fatal(err)
			}
			if got != phase {
				t.Fatalf("reconstructed phase = %q, want %q -- a failed preparer must not keep a run running", got, phase)
			}
			events, err := rd.Events()
			if err != nil {
				t.Fatal(err)
			}
			var sawFailureFact, sawFinished bool
			for _, ev := range events {
				if ev.Error != nil && ev.Error.Code == "run_abort_label_failed" {
					sawFailureFact = true
				}
				if ev.Type == journal.EventRunFinished && ev.Status == string(phase) {
					sawFinished = true
				}
			}
			if !sawFailureFact {
				t.Fatal("the preparer's failure fact must remain in the journal")
			}
			if !sawFinished {
				t.Fatal("run.finished must be appended even when the preparer failed")
			}
		})
	}
}

// TestFinishTerminalGatePhaseIsClaimReleasable ties the journal-level phase
// contract to the question the claim reaper actually asks ("is the holding run
// still running?") for every terminal gate outcome, including the case where
// no run.finished was ever written at all (a crash between the gate and
// terminalization).
func TestFinishTerminalGatePhaseIsClaimReleasable(t *testing.T) {
	tests := []struct {
		name         string
		events       []journal.Event
		wantReleased bool
	}{
		{
			name: "abort gate without run.finished releases the claim",
			events: []journal.Event{
				{Type: journal.EventRunStarted},
				{Type: journal.EventGateEvaluated, Gate: "merged-gate", Verdict: "fail", Target: journal.TargetAbort},
			},
			wantReleased: true,
		},
		{
			name: "escalate gate without run.finished releases the claim",
			events: []journal.Event{
				{Type: journal.EventRunStarted},
				{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "needs-changes", Target: journal.TargetEscalate},
			},
			wantReleased: true,
		},
		{
			name: "join gate keeps the claim held",
			events: []journal.Event{
				{Type: journal.EventRunStarted},
				{Type: journal.EventGateEvaluated, Gate: "branch-gate", Verdict: "pass", Target: journal.TargetJoin},
			},
			wantReleased: false,
		},
		{
			name: "a resume after an abort gate re-holds the claim",
			events: []journal.Event{
				{Type: journal.EventRunStarted},
				{Type: journal.EventGateEvaluated, Gate: "merged-gate", Target: journal.TargetAbort},
				{Type: journal.EventRunResumed},
			},
			wantReleased: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// PhaseFromEvents is the exact function claimHolderTerminal reaches
			// through Reader.Phase.
			phase := journal.PhaseFromEvents(tc.events)
			released := phase != journal.PhaseRunning
			if released != tc.wantReleased {
				t.Fatalf("phase = %q, claim released = %v, want released = %v", phase, released, tc.wantReleased)
			}
		})
	}
}
