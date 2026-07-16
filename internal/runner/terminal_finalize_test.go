package runner

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestFinishFinalizesEveryTerminalPhaseAfterJournaling(t *testing.T) {
	phases := []journal.RunPhase{
		journal.PhaseCompleted,
		journal.PhaseFailed,
		journal.PhaseAborted,
		journal.PhaseEscalated,
	}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			const runID = "terminal-run"
			runsDir := t.TempDir()
			jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = jr.Close() }()

			var finalized bool
			r := &Runner{cfg: Config{
				FinalizeTerminal: func(gotRunID string, gotPhase journal.RunPhase, _ *journal.Run) error {
					if gotRunID != runID || gotPhase != phase {
						t.Fatalf("finalizer got (%q, %q), want (%q, %q)", gotRunID, gotPhase, runID, phase)
					}
					rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
					if err != nil {
						t.Fatal(err)
					}
					journaledPhase, err := rd.Phase()
					if err != nil {
						t.Fatal(err)
					}
					if journaledPhase != phase {
						t.Fatalf("phase visible to finalizer = %q, want %q", journaledPhase, phase)
					}
					finalized = true
					return nil
				},
			}}

			res, err := r.finish(runID, jr, phase, "last-state", 3)
			if err != nil {
				t.Fatal(err)
			}
			if !finalized {
				t.Fatal("terminal finalizer was not called")
			}
			if res.Phase != phase || res.FinalState != "last-state" || res.Steps != 3 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

func TestFinishSurfacesFinalizerFailureWithTerminalResult(t *testing.T) {
	const runID = "terminal-finalizer-failure"
	jr, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: runID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()

	r := &Runner{cfg: Config{
		FinalizeTerminal: func(string, journal.RunPhase, *journal.Run) error {
			return errors.New("cleanup failed")
		},
	}}
	res, err := r.finish(runID, jr, journal.PhaseFailed, "implement", 1)
	if err == nil {
		t.Fatal("expected finalizer error")
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("result phase = %q, want failed", res.Phase)
	}
}

func TestFinishPreparesBeforeRunFinished(t *testing.T) {
	const runID = "terminal-preparer"
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()

	r := &Runner{cfg: Config{
		PrepareTerminal: func(gotRunID string, gotPhase journal.RunPhase, gotJournal *journal.Run) error {
			if gotRunID != runID || gotPhase != journal.PhaseAborted || gotJournal != jr {
				t.Fatalf("preparer got (%q, %q, %p)", gotRunID, gotPhase, gotJournal)
			}
			return gotJournal.Append(journal.Event{
				Type: journal.EventRefTouched,
				ExternalRef: &journal.ExternalRef{
					Provider: "github",
					Kind:     "branch",
					ID:       "goobers/implementation/terminal-preparer",
				},
				Runner: map[string]any{"operation": "delete", "outcome": "failed"},
			})
		},
	}}
	res, err := r.finish(runID, jr, journal.PhaseAborted, "review", 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q", res.Phase)
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[len(events)-2].Type != journal.EventRefTouched || events[len(events)-1].Type != journal.EventRunFinished {
		t.Fatalf("terminal event order = %+v", events)
	}
}
