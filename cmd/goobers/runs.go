package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// runSummary is the flat, journal-derived row goobers status prints per run.
type runSummary struct {
	RunID     string
	Workflow  string
	Gaggle    string
	Phase     journal.RunPhase
	StartedAt time.Time
}

// listRuns scans an instance's runs/ directory for run subdirectories and
// summarizes each via the journal reader. A missing runs/ directory yields an
// empty list, not an error (a freshly-init'd instance has none yet); an entry
// that isn't a readable run directory is skipped rather than failing the whole
// listing — status is best-effort over what's actually there.
func listRuns(runsDir string) ([]runSummary, error) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs directory: %w", err)
	}
	var out []runSummary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, entry.Name())
		reader, err := journal.OpenRead(dir)
		if err != nil {
			continue
		}
		id, err := reader.Identity()
		if err != nil {
			continue
		}
		out = append(out, runSummary{
			RunID:     id.RunID,
			Workflow:  id.Workflow,
			Gaggle:    id.Gaggle,
			Phase:     runPhase(reader),
			StartedAt: id.StartedAt,
		})
	}
	return out, nil
}

// runPhase prefers the state.json checkpoint (the fast path); if it's missing —
// e.g. a run whose first checkpoint hasn't landed yet — it falls back to the
// terminal run.finished event's Status, the same source of truth
// journal.Recover reconstructs the phase from. A run with neither is still
// running.
func runPhase(reader *journal.Reader) journal.RunPhase {
	if st, err := reader.State(); err == nil {
		return st.Phase
	}
	events, err := reader.Events()
	if err != nil {
		return journal.PhaseRunning
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != journal.EventRunFinished {
			continue
		}
		switch phase := journal.RunPhase(events[i].Status); phase {
		case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
			return phase
		}
	}
	return journal.PhaseRunning
}
