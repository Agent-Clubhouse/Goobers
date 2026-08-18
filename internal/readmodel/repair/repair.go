// Package repair is the rate-bounded bidirectional sweep (#1924, design §6.3).
//
// # Why the previous bound did not bind
//
// `reconcileIndex` skipped a run root whose mtime had not advanced, reasoning
// that it could not hold anything new. That reasoning is correct and useless:
// every new run bumps its parent's mtime, so on a live instance the root is
// always dirty and the scan reads all 40,665 entries every pass. A bound that
// only holds when nothing is happening is not a bound.
//
// Worse, it ran on the HTTP list path, and reached IngestRun →
// WithPruneProtection → acquireJournalLock. That is why all 40,665 run
// directories on the live instance contain a `.lock` file — including the 10,906
// with no run.yaml that can never be ingested. Every one was created by a read.
//
// # The replacement
//
// A fixed I/O budget, walked continuously and cycling, with a durable cursor.
// Cost is CONSTANT PER UNIT TIME. What scales with history is cycle time
// (H / rate), which is reported as lastCycleCompletedAt rather than hidden.
//
// The sweep never takes a journal lock, and never runs on a request path.
//
// # Bidirectional
//
// Not only "on disk but not projected". Also "projected but no longer on disk" —
// an operator rm, an abandoned restore, an unlink whose removal intent was lost.
// Without that direction, the claim that a projected row cannot outlive its
// journal is merely "unusual rather than impossible", and #1943 is exactly the
// case: a run whose journal is removed is silently reclassified as running and
// stays that way forever.
package repair

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
)

// Store is the read-only surface the sweep needs from the read model.
type Store interface {
	SweepCursor(ctx context.Context) (readmodel.SweepCursor, error)
	ProjectionFloor(ctx context.Context) (time.Time, bool, error)

	GetRun(ctx context.Context, runID string) (readmodel.RunRow, bool, error)

	IsUnpublished(ctx context.Context, runID string, mtime time.Time) (bool, error)

	Tombstoned(ctx context.Context, runID string) (bool, error)

	ProjectedRunIDsAfter(
		ctx context.Context,
		afterStartedAt time.Time,
		afterRunID string,
		before time.Time,
		limit int,
	) ([]readmodel.RunRow, error)
}

// Writer routes repair mutations through the read model's sole-writer loop.
type Writer interface {
	UpsertRun(ctx context.Context, p readmodel.Projection) error
	RemoveRun(ctx context.Context, runID string) error
	SaveSweepCursor(ctx context.Context, cursor readmodel.SweepCursor) error
	MarkUnpublished(ctx context.Context, runID string, mtime time.Time) error
	ClearUnpublished(ctx context.Context, runID string) error
	Tombstone(ctx context.Context, runID string, startedAt time.Time, reason string) error
}

// Watermarks is the intake surface the sweep consults.
//
// Only a reader: repair never records intake. Its job is to reconcile what other
// components recorded, and a repair pass that wrote watermarks could drive
// itself in a loop.
type Watermarks interface {
	Get(ctx context.Context, runID string) (intake.Marker, bool, error)
}

// Options configures a sweep.
type Options struct {
	// RunsDirs are the roots to walk.
	RunsDirs []string
	// EntriesPerSecond is the I/O budget. This is the bound: cost per unit time
	// is fixed, and cycle time is what varies with history.
	EntriesPerSecond int
	// BatchSize is how many entries one Step examines.
	BatchSize int
	Logger    *slog.Logger
	Now       func() time.Time
}

const (
	defaultEntriesPerSecond = 200
	defaultBatchSize        = 64
)

// Sweeper walks run directories and reconciles them against the read model.
type Sweeper struct {
	store      Store
	writer     Writer
	watermarks Watermarks
	options    Options
	stats      Stats
	stat       func(string) (os.FileInfo, error)
}

// Stats are the sweep's observable counters.
type Stats struct {
	EntriesExamined int
	Projected       int
	Removed         int
	SkippedFloor    int
	SkippedUnpub    int
	Tombstoned      int
	CyclesCompleted int
	Failures        int
}

// New constructs a sweeper.
func New(store Store, writer Writer, watermarks Watermarks, options Options) *Sweeper {
	if options.EntriesPerSecond <= 0 {
		options.EntriesPerSecond = defaultEntriesPerSecond
	}
	if options.BatchSize <= 0 {
		options.BatchSize = defaultBatchSize
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Sweeper{
		store: store, writer: writer, watermarks: watermarks, options: options,
		stat: os.Stat,
	}
}

// Run sweeps continuously until the context is cancelled.
//
// The pacing is the bound: BatchSize entries, then sleep long enough that the
// long-run rate is EntriesPerSecond. A sweep that fell behind does not "catch
// up" by going faster — that would make repair a source of the load it exists
// to survive.
func (s *Sweeper) Run(ctx context.Context) {
	interval := time.Duration(float64(time.Second) *
		float64(s.options.BatchSize) / float64(s.options.EntriesPerSecond))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Step(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.stats.Failures++
				s.options.Logger.Warn("repair sweep step failed", "error", err)
			}
		}
	}
}

// Step examines one batch and advances the cursor.
//
// Exported so a test can drive the sweep deterministically rather than by
// waiting on a ticker — a rate-bounded loop asserted with sleeps would be both
// slow and flaky.
func (s *Sweeper) Step(ctx context.Context) error {
	cursor, err := s.store.SweepCursor(ctx)
	if err != nil {
		return err
	}
	// Reserve half the batch for reverse progress; unused capacity immediately
	// returns to the forward walk. A one-entry batch alternates directions
	// because it cannot make progress in both within one Step.
	reverseLimit := s.options.BatchSize / 2
	if s.options.BatchSize == 1 && !cursor.ForwardNext {
		reverseLimit = 1
	}
	reverseExamined := 0
	if reverseLimit > 0 {
		reverseExamined, err = s.sweepReverse(ctx, &cursor, reverseLimit)
		if err != nil {
			return err
		}
	}
	if s.options.BatchSize == 1 {
		cursor.ForwardNext = !cursor.ForwardNext
	}
	forwardLimit := s.options.BatchSize - reverseExamined

	if cursor.Root == "" {
		cursor = s.beginCycle(cursor)
	}

	root, ok := s.resolveRoot(cursor.Root)
	if !ok {
		// The recorded root no longer exists — a gaggle was removed, or the
		// layout changed. Restart the cycle rather than failing: the cursor is a
		// position hint, not a fact about the world.
		return s.writer.SaveSweepCursor(ctx, s.beginCycle(cursor))
	}

	names, err := s.readBatch(root, cursor.AfterName, forwardLimit)
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.stats.EntriesExamined++
		cursor.EntriesThisCycle++
		if err := s.reconcile(ctx, filepath.Join(root, name), name); err != nil {
			// One directory's failure must not stop the walk; the cursor still
			// advances past it, or a single bad directory would wedge repair
			// forever at the same position.
			s.stats.Failures++
			s.options.Logger.Warn("repair reconcile failed", "run_id", name, "error", err)
		}
		cursor.AfterName = name
	}

	if len(names) < forwardLimit {
		// This root is exhausted. Move to the next, or complete the cycle.
		cursor = s.advanceRoot(cursor)
	}
	return s.writer.SaveSweepCursor(ctx, cursor)
}

// reconcile brings one on-disk directory into agreement with the read model.
func (s *Sweeper) reconcile(ctx context.Context, dir, runID string) error {
	info, err := s.stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}

	// The unpublished memo. 27% of directories on the live instance have no
	// run.yaml and can never be ingested; remembering them keyed by mtime turns
	// each into one stat rather than an open. Writing run.yaml bumps the
	// directory mtime, so promotion is detected rather than cached forever.
	remembered, err := s.store.IsUnpublished(ctx, runID, info.ModTime())
	if err != nil {
		return err
	}
	if remembered {
		s.stats.SkippedUnpub++
		return nil
	}
	if !journal.Recorded(dir) {
		s.stats.SkippedUnpub++
		return s.writer.MarkUnpublished(ctx, runID, info.ModTime())
	}
	// It was unpublished and now is not. Clear the memo so a later mtime change
	// is not measured against a stale entry.
	if err := s.writer.ClearUnpublished(ctx, runID); err != nil {
		return err
	}

	if _, projected, err := s.store.GetRun(ctx, runID); err != nil {
		return err
	} else if projected {
		// Already projected. The projector owns keeping it current; repair's job
		// is discovery, and reprojecting every already-known run would spend the
		// whole budget on work with a known-empty result.
		return nil
	}

	if tombstoned, err := s.store.Tombstoned(ctx, runID); err != nil {
		return err
	} else if tombstoned {
		// Deliberately aged out. Re-admitting it is the livelock: repair
		// projects, retention deletes, the next cycle repeats — consuming the
		// budget and flooding the change feed.
		s.stats.SkippedFloor++
		return nil
	}

	projection, found, err := s.project(dir)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	// The floor, and the resume override.
	//
	// A run older than the floor is skipped UNLESS it carries an intake marker.
	// An explicit resume (runner.ResumeFromTerminal) durably reopens a failed or
	// escalated run whose journal may predate the window, and a marker is
	// authority to re-admit: refusing would make a human action invisible in the
	// portal that prompted it.
	floor, hasFloor, err := s.store.ProjectionFloor(ctx)
	if err != nil {
		return err
	}
	if hasFloor && projection.Run.StartedAt.Before(floor) {
		resumed, err := s.hasMarker(ctx, runID)
		if err != nil {
			return err
		}
		if !resumed {
			s.stats.SkippedFloor++
			s.stats.Tombstoned++
			return s.writer.Tombstone(ctx, runID, projection.Run.StartedAt, "below_projection_floor")
		}
	}

	if err := s.writer.UpsertRun(ctx, projection); err != nil {
		return err
	}
	s.stats.Projected++
	return nil
}

// sweepReverse is the other direction: projected, but no longer on disk.
//
// This is what makes "a projected row cannot outlive its journal" a property
// rather than a hope, and it is the fix for #1943 — a run whose journal is
// removed is currently reclassified as running and stays that way forever,
// because nothing ever looks for rows whose source has vanished.
func (s *Sweeper) sweepReverse(
	ctx context.Context,
	cursor *readmodel.SweepCursor,
	limit int,
) (int, error) {
	if cursor.ReverseCycleBefore.IsZero() {
		cursor.ReverseCycleBefore = s.options.Now().UTC()
	}
	rows, err := s.store.ProjectedRunIDsAfter(
		ctx,
		cursor.ReverseAfterStartedAt,
		cursor.ReverseAfterRunID,
		cursor.ReverseCycleBefore,
		limit,
	)
	if err != nil {
		return 0, err
	}
	examined := 0
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return examined, err
		}
		s.stats.EntriesExamined++
		examined++
		if _, ok := s.locate(row.RunID); ok {
			cursor.ReverseAfterStartedAt = row.StartedAt
			cursor.ReverseAfterRunID = row.RunID
			continue
		}
		// Projected with no journal anywhere. Whether retention removed it or an
		// operator did, the row is now unsupported by any source, and §3.2 makes
		// journals authoritative — so the row goes and run.removed is published.
		if err := s.writer.RemoveRun(ctx, row.RunID); err != nil {
			return examined, err
		}
		cursor.ReverseAfterStartedAt = row.StartedAt
		cursor.ReverseAfterRunID = row.RunID
		s.stats.Removed++
	}
	if len(rows) < limit {
		cursor.ReverseAfterStartedAt = time.Time{}
		cursor.ReverseAfterRunID = ""
		cursor.ReverseCycleBefore = time.Time{}
	}
	return examined, nil
}

// project reads a run directory into a projection. Never takes a journal lock.
func (s *Sweeper) project(dir string) (readmodel.Projection, bool, error) {
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return readmodel.Projection{}, false, nil
	}
	identity, err := reader.Identity()
	if err != nil {
		return readmodel.Projection{}, false, nil
	}
	events, err := reader.Events()
	if err != nil {
		return readmodel.Projection{}, false,
			fmt.Errorf("repair: read events in %s: %w", dir, err)
	}
	projection, err := readmodel.ProjectRunFromJournal(reader, identity, events)
	if err != nil {
		return readmodel.Projection{}, false,
			fmt.Errorf("repair: project operator facts in %s: %w", dir, err)
	}
	return projection, true, nil
}

// hasMarker reports whether intake holds anything for this run.
func (s *Sweeper) hasMarker(ctx context.Context, runID string) (bool, error) {
	if s.watermarks == nil {
		return false, nil
	}
	_, found, err := s.watermarks.Get(ctx, runID)
	return found, err
}

// locate finds a run directory across the roots.
func (s *Sweeper) locate(runID string) (string, bool) {
	for _, root := range s.options.RunsDirs {
		candidate := filepath.Join(root, runID)
		if info, err := s.stat(candidate); err == nil && info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

// readBatch lists up to limit entries after a name, in lexicographic order.
//
// Sorting is what makes the cursor meaningful: os.ReadDir's order is not
// specified across platforms, and a walk that resumed "after X" in an unsorted
// listing could skip entries indefinitely.
func (s *Sweeper) readBatch(root, after string, limit int) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("repair: read %s: %w", root, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() > after {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) > limit {
		names = names[:limit]
	}
	return names, nil
}

// beginCycle starts a fresh pass at the first root.
func (s *Sweeper) beginCycle(cursor readmodel.SweepCursor) readmodel.SweepCursor {
	cursor.AfterName = ""
	cursor.EntriesThisCycle = 0
	cursor.CycleStartedAt = s.options.Now().UTC()
	if len(s.options.RunsDirs) > 0 {
		cursor.Root = s.options.RunsDirs[0]
	}
	return cursor
}

// advanceRoot moves to the next root, completing the cycle after the last.
func (s *Sweeper) advanceRoot(cursor readmodel.SweepCursor) readmodel.SweepCursor {
	for i, root := range s.options.RunsDirs {
		if root != cursor.Root {
			continue
		}
		if i+1 < len(s.options.RunsDirs) {
			cursor.Root = s.options.RunsDirs[i+1]
			cursor.AfterName = ""
			return cursor
		}
		break
	}
	// Cycle complete. Recording the completion time is what turns "how stale can
	// repair be" into a number rather than a promise.
	s.stats.CyclesCompleted++
	cursor.LastCycleCompletedAt = s.options.Now().UTC()
	return s.beginCycle(cursor)
}

// resolveRoot checks a recorded root is still one we walk.
func (s *Sweeper) resolveRoot(root string) (string, bool) {
	for _, candidate := range s.options.RunsDirs {
		if candidate == root {
			return candidate, true
		}
	}
	return "", false
}

// Stats returns the counters.
func (s *Sweeper) Stats() Stats { return s.stats }
