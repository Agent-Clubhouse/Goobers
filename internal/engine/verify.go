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

// emissionOnlyTypes are the event types a run can emit into its LIVE journal
// without a counterpart ever reaching Temporal history — so a live-vs-history
// diff that compares them positionally reports a difference on every run that
// produces one. They are the types dropUnmatchedEmissionOnly below is allowed
// to forgive a surplus of.
//
//   - artifact.recorded (#4244's dominant shape, 9,653 of 10,208 live-side
//     events). dispatchexec.go's artifact-recording comment documents the
//     mechanism, MEASURED on a live cluster: a stage's streams are emitted to
//     the live journal through the daemon's journal plane while only a POINTER
//     rides the surrendered result envelope into history. History replays that
//     pointer onto stage.finished's Artifacts field, so the re-projection has
//     no emission event to match. History CAN carry an artifact, though — an
//     opArtifact op re-records one, context manifests and gate verdicts among
//     them — so the asymmetry is per-ARTIFACT, not per-type. That is exactly
//     why the accommodation below is a surplus rule rather than a type
//     exclusion: excluding the type would also retire the check on every
//     artifact history does carry.
//   - agent.lifecycle (#3871's narrower shape, 555 events). Emitted by an
//     agentic pod stage. There is no history op that produces one at all:
//     validateOp fails a projection closed on the type, and no opArtifact /
//     opSpan route reaches it either.
//
// Deliberately NOT a change to journal.IsConformanceNormative: that set
// answers the cross-RUNNER conformance question, where both sides are
// live-authored, both emit artifact.recorded, and it is real coverage. The
// asymmetry belongs to the DS5 live-vs-history diff alone, so the
// accommodation does too. Keep this list SHORT: the failure mode of widening
// it is a real parity bug that never gets filed.
var emissionOnlyTypes = map[journal.EventType]bool{
	journal.EventArtifactRecorded: true,
	journal.EventAgentLifecycle:   true,
}

// dropUnmatchedEmissionOnly is the accommodation itself, and it is
// DIRECTIONAL by design — a blanket exclusion of the two types above would be
// over-broad, which is the one risk #4244 named.
//
// Only a live-side SURPLUS is forgiven: an emission-only event on the live
// side with no equal counterpart in the re-projection is dropped, because
// history structurally has no way to carry one. Everything else is left
// exactly as it was, and that is what keeps the guard a guard:
//
//   - History carries an artifact the live journal does not → the projected
//     event stays, the views differ, DS5 files. A live journal that LOST an
//     artifact is still caught.
//   - The same artifact differs across the two sides (name, stage, integrity,
//     digest — whatever conformance.go projects) → the live event matches
//     nothing, so it is dropped, and the projected event stays. The views
//     differ and DS5 files. Artifact CONTENT drift is still caught.
//   - A live-only emission → dropped, no filing. That is the ~98% false
//     positive, and it is the only case that changes.
//
// Note this needs no separate handling for agent.lifecycle: history has no op
// that produces one at all, so every such event is a live-side surplus and
// the same rule covers #3871's narrower shape.
//
// Matching is by whole normative event and by MULTIPLICITY (a run that emits
// the same artifact twice must still re-project twice), not by name, so the
// rule cannot be satisfied by a coincidence of one field.
func dropUnmatchedEmissionOnly(live, projected []journal.NormativeEvent) []journal.NormativeEvent {
	available := map[journal.NormativeEvent]int{}
	for _, ne := range projected {
		if emissionOnlyTypes[ne.Type] {
			available[ne]++
		}
	}
	out := make([]journal.NormativeEvent, 0, len(live))
	for _, ne := range live {
		if emissionOnlyTypes[ne.Type] {
			if available[ne] == 0 {
				continue
			}
			available[ne]--
		}
		out = append(out, ne)
	}
	return out
}

// diffNormativeViews compares two journals' conformance views, naming the
// first divergent position with both sides — the same shape the conformance
// harness reports, in production form.
func diffNormativeViews(live, projected []journal.Event) string {
	pv := journal.ConformanceView(projected)
	lv := dropUnmatchedEmissionOnly(journal.ConformanceView(live), pv)
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
