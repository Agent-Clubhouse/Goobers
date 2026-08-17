package readmodel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/journal"
)

// Building read.db from journals.
//
// §6.6 step 2: "Build it by rebuild-from-journals on first start". The journals
// are authoritative (§3.2), so the read model is entirely derived and this is
// both the initial construction and the recovery path.
//
// # Why this is a whole-store operation
//
// §5.1's split is what makes it affordable. The rebuild input is 191 MB of run
// events rather than the 2.5 GB that includes spans, and §2.6 notes the cost
// driver is file OPENS rather than bytes — 29,759 of them. Measured on the Wave
// 0 harness at 1x, rebuilding telemetry.db (which reads spans too) takes ~51 s;
// read.db reads strictly less.

// BuildResult reports what a build covered.
type BuildResult struct {
	// Scanned is directories examined; Projected is runs actually written.
	// They differ by the unpublished directories — 10,906 of 40,665 on the live
	// instance (27%), which have no run.yaml and can never be ingested.
	Scanned   int
	Projected int
	// Skipped counts directories that were not readable runs. A build does not
	// fail on them: an unpublished or half-created directory is an ordinary
	// state, not corruption, and refusing to build because one exists would make
	// the read model unavailable for a reason that does not affect any other run.
	Skipped int
}

// BuildFromJournals projects every readable run under runsDirs into the store.
//
// Runs are processed in sorted order so a build is deterministic run-over-run,
// which §14.9 requires: rebuilding from the same journals must produce identical
// rows, and processing order must not be able to change that.
//
// It is resumable by construction rather than by bookkeeping: UpsertRun is
// idempotent on last_seq, so a build interrupted halfway and restarted
// re-projects what it already did without changing it, and picks up the rest.
func (s *Store) BuildFromJournals(ctx context.Context, runsDirs []string) (BuildResult, error) {
	var result BuildResult
	for _, runsDir := range runsDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return result, fmt.Errorf("readmodel: read runs directory %s: %w", runsDir, err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)

		for _, name := range names {
			// Cancellation is honoured between runs rather than mid-run: a
			// partially-projected run would still be consistent (the upsert is
			// transactional) but stopping cleanly at a boundary keeps the
			// progress figures meaningful.
			if err := ctx.Err(); err != nil {
				return result, err
			}
			result.Scanned++
			projected, err := s.projectRunDir(ctx, filepath.Join(runsDir, name))
			if err != nil {
				return result, err
			}
			if projected {
				result.Projected++
			} else {
				result.Skipped++
			}
		}
	}
	return result, nil
}

// ProjectRunDir projects one run directory.
//
// Exported for the writer seam: the daemon projects a run at the moment it
// finishes, alongside the rollup ingest, so read.db is as fresh as telemetry.db
// rather than only as fresh as its last rebuild.
//
// This is the INTERIM mechanism. §6.1 requires the projector to be driven by a
// durable intake watermark rather than an in-process call from the writer,
// precisely so that runs written while the daemon was down are not invisible —
// which is the same limitation the current IngestRun-on-finish coupling has, and
// the reason repair exists. Wave 3 replaces it.
func (s *Store) ProjectRunDir(ctx context.Context, dir string) error {
	_, err := s.projectRunDir(ctx, dir)
	return err
}

// projectRunDir projects one run directory, reporting whether it was written.
func (s *Store) projectRunDir(ctx context.Context, dir string) (bool, error) {
	// An unpublished directory has no run.yaml and can never be ingested. This
	// check is what keeps a build from paying for the 27% of directories that
	// carry no identity — and, historically, from taking a journal lock on them
	// (the mechanism behind the stray .lock files in §2.4).
	if !journal.Recorded(dir) {
		return false, nil
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		// Not a readable run. Skipped rather than fatal: see BuildResult.Skipped.
		return false, nil
	}
	identity, err := reader.Identity()
	if err != nil {
		return false, nil
	}
	events, err := reader.Events()
	if err != nil {
		// A journal that exists but cannot be read IS worth surfacing — it is
		// corruption rather than absence, and silently skipping would make the
		// read model quietly incomplete, which §14.7 exists to prevent.
		return false, fmt.Errorf("readmodel: read events for %s: %w", identity.RunID, err)
	}

	projection, err := ProjectRunFromJournal(reader, identity, events)
	if err != nil {
		return false, err
	}
	if err := s.UpsertRun(ctx, projection); err != nil {
		return false, err
	}
	return true, nil
}
