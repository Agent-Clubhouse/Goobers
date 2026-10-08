package readmodel

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
)

// Startup reconciliation (#6895).
//
// Store open used to withdraw readiness whenever ANY row predated the current
// projection rules, and startup answered every unready store with a
// whole-journal build. A build cannot upgrade a row whose journal is gone or
// unreadable, so one such row made every restart re-read every journal until
// repair's reverse sweep removed it: tens of minutes of serial small reads on a
// large instance, re-projecting runs that were already current.
//
// Readiness now means only "a whole-journal build completed" (new store,
// replaying migration, interrupted build). A ready store's older rows are
// re-projected by locating each one's journal directly. That is not a weaker
// guarantee: a build skips the same unreadable journals, and runs whose
// journals changed while the daemon was down are the projector's restart pass
// and repair's job in both cases, exactly as for a store with no older rows.

// startupProgressEvery is how many runs pass between progress reports. At the
// cold-cache rates seen in production this is roughly one line every few tens
// of seconds: often enough to tell slow from stuck, rare enough not to flood.
const startupProgressEvery = 500

// ReadyResult reports what EnsureReady did.
type ReadyResult struct {
	// FullBuild is true when every journal was scanned rather than only the
	// rows projected by older rules.
	FullBuild bool
	// Considered is directories scanned (full build) or older rows found.
	Considered int
	// Projected is runs written from their journals.
	Projected int
	// Unresolved is older rows with no readable journal under any root. They
	// keep their projection until repair removes or re-projects them.
	Unresolved int
}

// EnsureReady brings the store up to date with this build before it is
// attached: a whole-journal build when the store is not ready, otherwise a
// re-projection of only the rows written by older projection rules.
//
// state must be the store's State as read after Open. report, when non-nil,
// receives operator-facing lines: why the work is needed, periodic done/total
// counts, and a summary. A ready store with no older rows reports nothing.
func (s *Store) EnsureReady(ctx context.Context, state State, runsDirs []string, report func(string)) (ReadyResult, error) {
	if state.Ready {
		return s.reprojectStaleRuns(ctx, runsDirs, report)
	}
	result, err := s.fullBuild(ctx, runsDirs, report)
	if err != nil {
		return result, err
	}
	return result, s.MarkReady(ctx)
}

func (s *Store) fullBuild(ctx context.Context, runsDirs []string, report func(string)) (ReadyResult, error) {
	reportLine(report, "read model: full journal build required (new store, replaying migration, or interrupted build)")
	built, err := s.buildFromJournals(ctx, runsDirs, throttledProgress(report, "read model build: scanned %d/%d run directories"))
	result := ReadyResult{FullBuild: true, Considered: built.Scanned, Projected: built.Projected}
	if err != nil {
		return result, err
	}
	reportLine(report, fmt.Sprintf("read model build complete: %d run directories scanned, %d projected, %d skipped",
		built.Scanned, built.Projected, built.Skipped))
	return result, nil
}

// reprojectStaleRuns re-projects only the rows written by older projection
// rules, locating each run's journal directly instead of scanning every root.
func (s *Store) reprojectStaleRuns(ctx context.Context, runsDirs []string, report func(string)) (ReadyResult, error) {
	runIDs, err := s.staleRunIDs(ctx)
	if err != nil || len(runIDs) == 0 {
		return ReadyResult{}, err
	}
	result := ReadyResult{Considered: len(runIDs)}
	reportLine(report, fmt.Sprintf(
		"read model: %d run(s) were projected by older rules; re-projecting only those (no full journal scan)", len(runIDs)))
	progress := throttledProgress(report, "read model re-projection: %d/%d runs")
	for i, runID := range runIDs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		projected := false
		if dir, ok := locateRunDir(runID, runsDirs); ok {
			if projected, err = s.projectRunDir(ctx, dir); err != nil {
				return result, err
			}
		}
		if projected {
			result.Projected++
		} else {
			result.Unresolved++
		}
		progress(i+1, len(runIDs))
	}
	reportLine(report, fmt.Sprintf(
		"read model re-projection complete: %d re-projected, %d without a readable journal (left for repair)",
		result.Projected, result.Unresolved))
	return result, nil
}

// staleRunIDs lists runs whose rows predate currentProjectionVersion.
func (s *Store) staleRunIDs(ctx context.Context) ([]string, error) {
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx,
		`SELECT run_id FROM run WHERE projection_version < ? ORDER BY run_id`, currentProjectionVersion)
	if err != nil {
		return nil, fmt.Errorf("readmodel: list stale projections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return nil, fmt.Errorf("readmodel: scan stale projection: %w", err)
		}
		out = append(out, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: stale projection rows: %w", err)
	}
	return out, nil
}

// locateRunDir finds the first root holding a published journal for runID.
// A run's directory is named by its run ID, which a build relies on too; an ID
// that is not a single path element can never name one.
func locateRunDir(runID string, runsDirs []string) (string, bool) {
	if runID == "" || runID == "." || runID == ".." || filepath.Base(runID) != runID {
		return "", false
	}
	for _, root := range runsDirs {
		dir := filepath.Join(root, runID)
		if journal.Recorded(dir) {
			return dir, true
		}
	}
	return "", false
}

func reportLine(report func(string), line string) {
	if report != nil {
		report(line)
	}
}

// throttledProgress reports every startupProgressEvery items and at the end.
func throttledProgress(report func(string), format string) func(done, total int) {
	return func(done, total int) {
		if report != nil && (done%startupProgressEvery == 0 || done == total) {
			report(fmt.Sprintf(format, done, total))
		}
	}
}
