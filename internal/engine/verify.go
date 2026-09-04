package engine

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// verify.go is DS5's cross-check half: with live authorship (DS4) owning the
// journal, the retained history projection doubles as the only independent
// reconstruction of a run — so a live-authored journal is VERIFIED against a
// history re-projection instead of being overwritten, and a divergence is
// filed to a named channel (the #2871 parity ledger's feed), never silently
// repaired.

// maxDivergenceDetail bounds a filed divergence message so the annotation
// channel (an instance-journal event) can never be bloated by a pathological
// diff (#1166's lesson applied here).
const maxDivergenceDetail = 1500

// DiffLiveJournal re-projects proj into a scratch directory through the same
// ProjectRun machinery the repair path uses — there is deliberately no second
// projection implementation to drift — and diffs the live journal's normative
// view (journal.ConformanceView, the single sanctioned comparison surface)
// against the re-projection's. Returns "" when the views agree, else a
// bounded human-readable description of the first divergence.
//
// opts should carry the same projection options (notably WithSpanSource) the
// live writer runs with, so span availability — a property of the
// environment, not of the run — cannot masquerade as a divergence.
func DiffLiveJournal(liveEvents []journal.Event, proj JournalProjection, opts ...ProjectOption) (string, error) {
	stagingRoot, err := os.MkdirTemp("", "goobers-journal-verify-")
	if err != nil {
		return "", fmt.Errorf("engine: create verification staging root: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingRoot) }()

	// Project the journal under a runs/ child of the verification root so the
	// sibling staging directories the journal machinery creates live under the
	// same tree as the temp root. Without that nesting, the lock and creation
	// staging dirs are siblings of the root and survive the cleanup pass.
	runsDir := filepath.Join(stagingRoot, "runs")
	dir, err := ProjectRun(runsDir, proj, opts...)
	if err != nil {
		return "", fmt.Errorf("engine: re-project run %q for verification: %w", proj.Identity.RunID, err)
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		return "", fmt.Errorf("engine: open verification re-projection: %w", err)
	}
	projected, err := rd.Events()
	if err != nil {
		return "", fmt.Errorf("engine: read verification re-projection: %w", err)
	}
	return boundDetail(diffNormativeViews(liveEvents, projected)), nil
}

// diffNormativeViews compares two journals' conformance views, naming the
// first divergent position with both sides — the same shape the conformance
// harness reports, in production form.
func diffNormativeViews(live, projected []journal.Event) string {
	lv, pv := journal.ConformanceView(live), journal.ConformanceView(projected)
	limit := min(len(lv), len(pv))
	for i := 0; i < limit; i++ {
		if lv[i] != pv[i] {
			return fmt.Sprintf(
				"normative event %d diverges:\n  live:      %s\n  projected: %s",
				i+1, lv[i], pv[i])
		}
	}
	if len(lv) != len(pv) {
		longerName, longer := "projected", pv
		if len(lv) > len(pv) {
			longerName, longer = "live", lv
		}
		return fmt.Sprintf(
			"normative view lengths diverge (live %d, projected %d); first extra %s event: %s",
			len(lv), len(pv), longerName, longer[limit])
	}
	return ""
}

func boundDetail(detail string) string {
	if len(detail) <= maxDivergenceDetail {
		return detail
	}
	return detail[:maxDivergenceDetail] + " …(truncated)"
}

// journalInspection is one read of an on-disk journal, shared by the
// reconciler's routing decisions so the events file is opened once.
type journalInspection struct {
	events       []journal.Event
	complete     bool
	liveAuthored bool
}

func inspectJournal(dir string) (journalInspection, error) {
	rd, err := journal.OpenRead(dir)
	if err != nil {
		return journalInspection{}, err
	}
	events, err := rd.Events()
	if err != nil {
		return journalInspection{}, err
	}
	inspection := journalInspection{events: events, liveAuthored: livejournal.Authored(events)}
	if len(events) == 0 {
		return inspection, nil
	}
	last := events[len(events)-1]
	if last.Type == journal.EventRunFinished {
		switch journal.RunPhase(last.Status) {
		case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
			inspection.complete = true
		}
	}
	return inspection, nil
}
