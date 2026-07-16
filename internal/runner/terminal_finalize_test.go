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
				FinalizeTerminal: func(gotRunID string, gotPhase journal.RunPhase) error {
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
		FinalizeTerminal: func(string, journal.RunPhase) error {
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
