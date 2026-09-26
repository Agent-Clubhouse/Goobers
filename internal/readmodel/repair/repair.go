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
	SweepRootCursors(ctx context.Context) ([]readmodel.SweepRootCursor, error)
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
	SaveSweepRootCursor(ctx context.Context, cursor readmodel.SweepRootCursor) error
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
	// ResolveRunsDirs, when set, replaces RunsDirs with the current journal
	// roots. A discovery error must stop the sweep, not imply missing journals.
	ResolveRunsDirs func(context.Context) ([]string, error)
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
	Refreshed       int
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
	runsDirs, err := s.runsDirs(ctx)
	if err != nil {
		return err
	}
	cursor, err := s.store.SweepCursor(ctx)
	if err != nil {
		return err
	}
	rootCursors, err := s.store.SweepRootCursors(ctx)
	if err != nil {
		return err
	}
	byRoot := make(map[string]readmodel.SweepRootCursor, len(rootCursors))
	for _, rootCursor := range rootCursors {
		byRoot[rootCursor.Root] = rootCursor
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
		reverseExamined, err = s.sweepReverse(ctx, &cursor, reverseLimit, runsDirs)
		if err != nil {
			return err
		}
	}
	if s.options.BatchSize == 1 {
		cursor.ForwardNext = !cursor.ForwardNext
	}
	forwardLimit := s.options.BatchSize - reverseExamined
	if err := s.sweepForward(ctx, &cursor, byRoot, forwardLimit, runsDirs); err != nil {
		return err
	}
	s.updateAggregateCursor(&cursor, byRoot, runsDirs)
	return s.writer.SaveSweepCursor(ctx, cursor)
}

// sweepForward spends this Step's forward budget fairly across configured run
// roots. A root that fills the budget yields to the next root on the next Step;
// an exhausted root passes its unused budget on immediately. Each root keeps a
// separate durable position, so yielding never restarts its walk.
func (s *Sweeper) sweepForward(
	ctx context.Context,
	cursor *readmodel.SweepCursor,
	byRoot map[string]readmodel.SweepRootCursor,
	limit int,
	runsDirs []string,
) error {
	if limit <= 0 || len(runsDirs) == 0 {
		return nil
	}
	rootIndex := s.rootIndex(cursor.Root, runsDirs)
	if rootIndex < 0 {
		rootIndex = 0
	}
	remaining := limit
	for visited := 0; visited < len(runsDirs) && remaining > 0; visited++ {
		root := runsDirs[rootIndex]
		rootCursor, ok := byRoot[root]
		if !ok {
			rootCursor = readmodel.SweepRootCursor{Root: root}
		}
		examined, err := s.sweepForwardRoot(ctx, &rootCursor, remaining)
		if err != nil {
			return err
		}
		if err := s.writer.SaveSweepRootCursor(ctx, rootCursor); err != nil {
			return err
		}
		byRoot[root] = rootCursor
		remaining -= examined
		rootIndex = (rootIndex + 1) % len(runsDirs)
		cursor.Root = runsDirs[rootIndex]
		// Filling the available budget is the normal case. Stop here so the
		// next Step begins at the next root instead of letting the first large
		// gaggle monopolize every tick.
		if remaining == 0 {
			break
		}
	}
	return nil
}

func (s *Sweeper) sweepForwardRoot(
	ctx context.Context,
	cursor *readmodel.SweepRootCursor,
	limit int,
) (int, error) {
	if cursor.CycleStartedAt.IsZero() {
		cursor.CycleStartedAt = s.options.Now().UTC()
	}
	names, err := s.readBatch(cursor.Root, cursor.AfterName, limit)
	if err != nil {
		return 0, err
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		s.stats.EntriesExamined++
		cursor.EntriesThisCycle++
		if err := s.reconcile(ctx, filepath.Join(cursor.Root, name), name); err != nil {
			// One directory's failure must not stop the walk; the cursor still
			// advances past it, or a single bad directory would wedge repair
			// forever at the same position.
			s.stats.Failures++
			s.options.Logger.Warn("repair reconcile failed",
				"root", cursor.Root, "run_id", name, "error", err)
		}
		cursor.AfterName = name
	}
	if len(names) < limit {
		cursor.LastCycleCompletedAt = s.options.Now().UTC()
		cursor.AfterName = ""
		cursor.EntriesThisCycle = 0
		cursor.CycleStartedAt = s.options.Now().UTC()
	}
	return len(names), nil
}

// updateAggregateCursor preserves the singleton cursor as the global reverse
// position and freshness summary. A global cycle completes only when every
// currently configured runs root has completed a forward cycle.
func (s *Sweeper) updateAggregateCursor(
	cursor *readmodel.SweepCursor,
	byRoot map[string]readmodel.SweepRootCursor,
	runsDirs []string,
) {
	cursor.AfterName = ""
	cursor.CycleStartedAt = time.Time{}
	cursor.EntriesThisCycle = 0
	allCompleted := len(runsDirs) > 0
	var completedAt time.Time
	for _, root := range runsDirs {
		rootCursor, ok := byRoot[root]
		if !ok {
			allCompleted = false
			continue
		}
		if cursor.CycleStartedAt.IsZero() ||
			(!rootCursor.CycleStartedAt.IsZero() && rootCursor.CycleStartedAt.Before(cursor.CycleStartedAt)) {
			cursor.CycleStartedAt = rootCursor.CycleStartedAt
		}
		cursor.EntriesThisCycle += rootCursor.EntriesThisCycle
		if rootCursor.LastCycleCompletedAt.IsZero() {
			allCompleted = false
			continue
		}
		if completedAt.IsZero() || rootCursor.LastCycleCompletedAt.Before(completedAt) {
			completedAt = rootCursor.LastCycleCompletedAt
		}
	}
	if allCompleted && completedAt.After(cursor.LastCycleCompletedAt) {
		cursor.LastCycleCompletedAt = completedAt
		s.stats.CyclesCompleted++
	}
	if next, ok := byRoot[cursor.Root]; ok {
		cursor.AfterName = next.AfterName
	}
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

	if row, projected, err := s.store.GetRun(ctx, runID); err != nil {
		return err
	} else if projected {
		// Already projected. The projector owns keeping it current; repair's job
		// is discovery, and reprojecting every already-known run would spend the
		// whole budget on work with a known-empty result.
		//
		// One exception, and it is not a budget problem: a row that still claims
		// `running`. See refreshStaleRunning.
		return s.refreshStaleRunning(ctx, dir, runID, row)
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

// refreshStaleRunning re-projects a row that claims `running` when its journal
// has in fact reached a terminal event.
//
// # Why repair has to do this at all
//
// The projector is watermark-driven: a run is re-read only because something
// recorded intake for it. Everything that appends to a run journal under the
// daemon carries that observer — except, until #5278, the ad-hoc terminalizer
// the stalled-run sweep builds for a run no live Runner owns. Its run.finished
// landed with nobody watching, so no watermark was recorded, so the projector
// never looked again. And the forward walk above only DISCOVERS unprojected
// runs, so nothing else ever revisited the row either: it stayed `running`
// while its journal said otherwise, for as long as the row existed. Four rows
// on the cloud instance sat that way for up to sixteen days, which is what
// made the stuck-run population look far worse than it was and hid the three
// genuinely stalled runs among the false positives.
//
// Wiring the observer stops new rows going stale. This heals the ones already
// stale — including any left by a writer that is still missing its observer,
// which is the property worth having: the read model converges on the journal
// without depending on every writer remembering to say so.
//
// # Why this does not reopen the budget argument
//
// The skip above exists because re-projecting every known run would spend the
// whole budget re-deriving terminal rows that cannot have changed. This costs
// one bounded tail read, and only for rows that claim `running` — a handful on
// any instance, against the tens of thousands of terminal rows that still cost
// exactly one GetRun. PhaseBounded reads from the END of the journal and stops
// at the decisive record (#2755), so the common answer comes from the last few
// kilobytes rather than a full parse.
//
// A row that is genuinely running is re-checked each cycle and left alone. That
// is the correct outcome, not waste: it is also how a row whose run ended
// without any watermark at all gets noticed.
func (s *Sweeper) refreshStaleRunning(ctx context.Context, dir, runID string, row readmodel.RunRow) error {
	if row.Phase != journal.PhaseRunning {
		return nil
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		// Unreadable is not a disagreement. The reverse walk owns rows whose
		// journal has gone missing; anything else is for the next cycle.
		return nil
	}
	phase, err := reader.PhaseBounded(ctx)
	if err != nil {
		return fmt.Errorf("repair: read phase in %s: %w", dir, err)
	}
	if phase == journal.PhaseRunning {
		return nil
	}
	// The journal is terminal and the row is not. §3.2 makes the journal
	// authoritative, so re-derive the whole row from it rather than patching the
	// phase column: the terminal event settles finishedAt, currentStage and the
	// outcome fields together, and a row carrying a terminal phase beside
	// running-shaped values would be a third state neither source describes.
	projection, found, err := s.project(dir)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := s.writer.UpsertRun(ctx, projection); err != nil {
		return err
	}
	s.stats.Refreshed++
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
	runsDirs []string,
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
	refreshedRoots := false
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return examined, err
		}
		s.stats.EntriesExamined++
		examined++
		_, found := s.locate(row.RunID, runsDirs)
		if !found && !refreshedRoots && s.options.ResolveRunsDirs != nil {
			// A new root may have been projected after Step's snapshot.
			// Refresh after selecting candidates before declaring a journal gone.
			runsDirs, err = s.runsDirs(ctx)
			if err != nil {
				return examined, err
			}
			refreshedRoots = true
			_, found = s.locate(row.RunID, runsDirs)
		}
		if found {
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
func (s *Sweeper) locate(runID string, runsDirs []string) (string, bool) {
	for _, root := range runsDirs {
		candidate := filepath.Join(root, runID)
		if info, err := s.stat(candidate); err == nil && info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func (s *Sweeper) runsDirs(ctx context.Context) ([]string, error) {
	if s.options.ResolveRunsDirs == nil {
		return s.options.RunsDirs, nil
	}
	dirs, err := s.options.ResolveRunsDirs(ctx)
	if err != nil {
		return nil, fmt.Errorf("repair: resolve runs directories: %w", err)
	}
	return dirs, nil
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

// rootIndex resolves the durable round-robin position against the currently
// configured roots. A removed root returns -1 and the caller restarts at the
// first current root; its old per-root cursor is harmless retained history.
func (s *Sweeper) rootIndex(target string, runsDirs []string) int {
	for i, root := range runsDirs {
		if root == target {
			return i
		}
	}
	return -1
}

// Stats returns the counters.
func (s *Sweeper) Stats() Stats { return s.stats }
