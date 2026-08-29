// Package projector is the sole writer of projected facts (#1923, design §6.1,
// §6.2, §6.4).
//
// # What it replaces
//
// Today the coupling is inverted: `cmd/goobers/runnerwiring.go` and
// `cmd/goobers/daemon.go` call `IngestRun` from the WRITER, in-process, after a
// run finishes. That dependency does not survive separating execution from
// serving — and it has a defect that shows up long before any separation, which
// is that a run written while the daemon is down is never ingested at all and
// nothing notices.
//
// The projector inverts it: execution records a watermark in intake.db and
// forgets about it; the projector discovers work and applies it on its own
// schedule.
//
// # One serialized commit loop
//
// Journals may be read and work prepared concurrently. The COMMIT of
// (run row, run_stage rows, change row) — one read.db transaction — is
// serialized through a single goroutine.
//
// This is not a performance choice, it is a correctness one. Without it two
// workers allocate change sequences 10 and 11; 11 commits first; a client
// advances its cursor past 11; then 10 commits, and `WHERE seq > 11` never
// returns it. The client has silently missed a transition and cannot detect
// that it did — the cursor still parses, still validates, and still points into
// the feed. §8.2's rule that a client discards lower positions makes the loss
// permanent.
//
// # The acknowledgement is outside the transaction
//
// intake.db is a separate file and SQLite's WAL gives no atomic commit across
// databases, so the ack is a guarded post-commit write. See the intake package.
package projector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
)

// Store is the projected-facts surface the projector needs.
//
// Narrower than readmodel.Backend: the projector writes runs and reads back the
// little it needs to decide what to reproject. It has no list or aggregate
// method, because serving is not its job.
type Store interface {
	UpsertRun(ctx context.Context, p Projection) error
	GetRun(ctx context.Context, runID string) (readmodel.RunRow, bool, error)
	NonTerminalRuns(ctx context.Context, limit int) ([]readmodel.RunRow, error)
	RemoveRun(ctx context.Context, runID string) error
	SaveSweepCursor(ctx context.Context, cursor readmodel.SweepCursor) error
	MarkUnpublished(ctx context.Context, runID string, mtime time.Time) error
	ClearUnpublished(ctx context.Context, runID string) error
	Tombstone(ctx context.Context, runID string, startedAt time.Time, reason string) error
	SetProjectionFloor(ctx context.Context, floor time.Time) error
	PruneChangeFeed(ctx context.Context, keep int) (int64, error)
}

// Projection is the unit the projector commits. Aliased so callers and tests do
// not need to name two packages for one concept.
type Projection = readmodel.Projection

// Intake is the watermark surface, narrowed to what the projector uses.
type Intake interface {
	Pending(ctx context.Context, limit int) ([]intake.Marker, error)
	Ack(ctx context.Context, runID string, projectedSeq uint64) (bool, error)
	AckRemoval(ctx context.Context, runID string) error
}

// Notifier is woken after each committed projection.
//
// Called AFTER the transaction commits, never before: a subscriber woken early
// reads the feed, finds nothing, and goes back to sleep — and the change it was
// woken for then waits for some unrelated later commit to be noticed.
type Notifier interface{ Notify() }

// Options configures a projector.
type Options struct {
	// Feed is woken after each commit. Optional; without it subscribers fall
	// back to their own polling, which is slower but not wrong.
	Feed Notifier
	// RunsDirs are the roots holding run directories.
	RunsDirs []string
	// Workers bounds concurrent journal reading and projection preparation. The
	// COMMIT is serialized regardless; this only widens the expensive part,
	// which is parsing event tails.
	Workers int
	// DrainLimit bounds one intake pass.
	DrainLimit int
	// Interval is how often the projector drains intake when idle.
	Interval time.Duration
	// Logger receives degraded conditions. Never nil after New.
	Logger *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time
}

const (
	defaultWorkers    = 4
	defaultDrainLimit = 256
	defaultInterval   = 2 * time.Second
	// restartReprojectLimit bounds the restart pass. §6.1 requires restart cost
	// to be O(active + pending) rather than a scan; the limit is what makes that
	// a property of the code rather than of the corpus.
	restartReprojectLimit = 1024
)

// Projector applies journal facts into the read model.
type Projector struct {
	store   Store
	intake  Intake
	options Options

	// commits serializes the commit loop. Preparation fans out; this is the
	// single point through which every write passes, and it is why change
	// sequences are handed out in commit order.
	commits chan commitRequest

	// stats are observable counters for the freshness surface.
	mu    sync.Mutex
	stats Stats

	started bool
	stop    chan struct{}
	done    chan struct{}

	// prepareForTest replaces journal reading. Tests that are about the commit
	// loop, the acknowledgement protocol, or restart bounds should not also have
	// to construct real journals on disk — and a test that did would be proving
	// journal parsing rather than the property it names.
	//
	// The differential coverage against real journals lives in test/scale, which
	// is where it belongs: there the input is a generated instance rather than a
	// hand-built fixture.
	prepareForTest func(ctx context.Context, runID string) (Projection, bool, error)
}

// Stats are the projector's observable counters.
type Stats struct {
	Projected       int
	Removed         int
	Superseded      int
	AckFailures     int
	ProjectFailures int
	// LastDrainAt is when the last intake pass completed.
	LastDrainAt time.Time
}

// commitRequest is one prepared projection awaiting its turn at the commit loop.
type commitRequest struct {
	write  func(context.Context, Store) error
	notify bool
	result chan error
}

// New constructs a projector.
func New(store Store, watermarks Intake, options Options) *Projector {
	if options.Workers <= 0 {
		options.Workers = defaultWorkers
	}
	if options.DrainLimit <= 0 {
		options.DrainLimit = defaultDrainLimit
	}
	if options.Interval <= 0 {
		options.Interval = defaultInterval
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Projector{
		store:   store,
		intake:  watermarks,
		options: options,
		commits: make(chan commitRequest),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start begins the commit loop and the drain schedule.
//
// It returns a stop function rather than requiring the caller to hold the
// projector, so a daemon can wire it in one line and shut it down in one.
func (p *Projector) Start(ctx context.Context) func() {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return func() {}
	}
	p.started = true
	p.mu.Unlock()

	loopCtx, cancel := context.WithCancel(ctx)
	go p.commitLoop(loopCtx)
	go p.schedule(loopCtx)

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			close(p.stop)
			<-p.done
		})
	}
}

// commitLoop is the single goroutine through which every write passes.
//
// Serialization lives here and nowhere else. Every other part of the projector
// may run concurrently; this is the point that makes change sequences monotone
// in commit order.
func (p *Projector) commitLoop(ctx context.Context) {
	defer close(p.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case request := <-p.commits:
			err := request.write(ctx, p.store)
			// Notify only on a successful commit, and only after it. A failed
			// projection has published nothing, so waking subscribers would send
			// them to read a change that does not exist.
			if err == nil && request.notify && p.options.Feed != nil {
				p.options.Feed.Notify()
			}
			request.result <- err
		}
	}
}

// commit hands a prepared projection to the serialized loop and waits for it.
func (p *Projector) commit(ctx context.Context, request commitRequest) error {
	request.result = make(chan error, 1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.commits <- request:
	}
	// Once accepted, the write closure may refer to caller-owned result state.
	// Wait for it to finish even if the context is canceled so the caller cannot
	// observe that state while the commit loop is still updating it.
	return <-request.result
}

// UpsertRun commits a prepared projection through the sole-writer loop.
func (p *Projector) UpsertRun(ctx context.Context, projection Projection) error {
	return p.commit(ctx, commitRequest{
		write: func(ctx context.Context, store Store) error {
			return store.UpsertRun(ctx, projection)
		},
		notify: true,
	})
}

// RemoveRun removes a projected run through the sole-writer loop.
func (p *Projector) RemoveRun(ctx context.Context, runID string) error {
	return p.commit(ctx, commitRequest{
		write: func(ctx context.Context, store Store) error {
			return store.RemoveRun(ctx, runID)
		},
		notify: true,
	})
}

// SaveSweepCursor commits repair progress through the sole-writer loop.
func (p *Projector) SaveSweepCursor(ctx context.Context, cursor readmodel.SweepCursor) error {
	return p.commit(ctx, commitRequest{write: func(ctx context.Context, store Store) error {
		return store.SaveSweepCursor(ctx, cursor)
	}})
}

// MarkUnpublished commits an unpublished-directory memo through the sole-writer loop.
func (p *Projector) MarkUnpublished(ctx context.Context, runID string, mtime time.Time) error {
	return p.commit(ctx, commitRequest{write: func(ctx context.Context, store Store) error {
		return store.MarkUnpublished(ctx, runID, mtime)
	}})
}

// ClearUnpublished removes an unpublished-directory memo through the sole-writer loop.
func (p *Projector) ClearUnpublished(ctx context.Context, runID string) error {
	return p.commit(ctx, commitRequest{write: func(ctx context.Context, store Store) error {
		return store.ClearUnpublished(ctx, runID)
	}})
}

// Tombstone records an intentionally excluded run through the sole-writer loop.
func (p *Projector) Tombstone(
	ctx context.Context,
	runID string,
	startedAt time.Time,
	reason string,
) error {
	return p.commit(ctx, commitRequest{write: func(ctx context.Context, store Store) error {
		return store.Tombstone(ctx, runID, startedAt, reason)
	}})
}

// SetProjectionFloor advances retention through the sole-writer loop.
func (p *Projector) SetProjectionFloor(ctx context.Context, floor time.Time) error {
	return p.commit(ctx, commitRequest{write: func(ctx context.Context, store Store) error {
		return store.SetProjectionFloor(ctx, floor)
	}})
}

// PruneChangeFeed removes expired feed entries through the sole-writer loop.
func (p *Projector) PruneChangeFeed(ctx context.Context, keep int) (int64, error) {
	var pruned int64
	err := p.commit(ctx, commitRequest{write: func(ctx context.Context, store Store) error {
		var err error
		pruned, err = store.PruneChangeFeed(ctx, keep)
		return err
	}})
	return pruned, err
}

// schedule drains intake on an interval.
func (p *Projector) schedule(ctx context.Context) {
	ticker := time.NewTicker(p.options.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := p.Drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
				p.options.Logger.Warn("projector drain failed", "error", err)
			}
		}
	}
}

// Drain applies every pending intake marker, and reports how many it handled.
//
// This is the ordinary path: a writer recorded a watermark, and the projector
// turns it into projected facts. Preparation fans out across workers; commits
// funnel through the serialized loop.
func (p *Projector) Drain(ctx context.Context) (int, error) {
	markers, err := p.intake.Pending(ctx, p.options.DrainLimit)
	if err != nil {
		return 0, err
	}
	if len(markers) == 0 {
		p.recordDrain()
		return 0, nil
	}

	var (
		wg        sync.WaitGroup
		semaphore = make(chan struct{}, p.options.Workers)
		handled   int
		mu        sync.Mutex
	)
	for _, marker := range markers {
		select {
		case <-ctx.Done():
			wg.Wait()
			return handled, ctx.Err()
		case semaphore <- struct{}{}:
		}
		wg.Add(1)
		go func(marker intake.Marker) {
			defer wg.Done()
			defer func() { <-semaphore }()
			if err := p.applyMarker(ctx, marker); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				// One run's failure must not abort the pass. The read model is
				// derived, so a run that fails to project is rediscovered by the
				// repair sweep — whereas aborting would leave every later marker
				// in the batch unprocessed for the same reason.
				p.options.Logger.Warn("project run failed",
					"run_id", marker.RunID, "error", err)
				p.bump(func(s *Stats) { s.ProjectFailures++ })
				return
			}
			mu.Lock()
			handled++
			mu.Unlock()
		}(marker)
	}
	wg.Wait()
	p.recordDrain()
	return handled, ctx.Err()
}

// applyMarker projects (or removes) one run and acknowledges it.
func (p *Projector) applyMarker(ctx context.Context, marker intake.Marker) error {
	if marker.Removing {
		return p.applyRemoval(ctx, marker)
	}

	projection, found, err := p.prepare(ctx, marker.RunID)
	if err != nil {
		return err
	}
	if !found {
		// A watermark with no readable journal. This is not corruption — the
		// journal may have been removed between the observation and now, with
		// the removal intent lost. Leaving the marker lets the repair sweep,
		// which can see the whole picture, decide; consuming it here would erase
		// the only record that anything was ever expected.
		return nil
	}

	if err := p.UpsertRun(ctx, projection); err != nil {
		return err
	}
	p.bump(func(s *Stats) { s.Projected++ })

	// Post-commit, guarded. A newer append left a higher source_seq, so the
	// delete no-ops and the projector revisits — which is why this reports
	// "superseded" rather than treating a surviving marker as a failure.
	acked, err := p.intake.Ack(ctx, marker.RunID, projection.Run.LastSeq)
	if err != nil {
		// The projection committed; only the acknowledgement failed. The marker
		// survives and the next pass reprojects idempotently, so this is a
		// degraded condition rather than an error for the run.
		p.bump(func(s *Stats) { s.AckFailures++ })
		p.options.Logger.Warn("intake acknowledgement failed",
			"run_id", marker.RunID, "seq", projection.Run.LastSeq, "error", err)
		return nil
	}
	if !acked {
		p.bump(func(s *Stats) { s.Superseded++ })
	}
	return nil
}

// applyRemoval handles a marker carrying retention's removal intent.
//
// The ordering is intent → unlink → project → confirm, and this is the project
// step. The stale-intent case is handled upstream in intake.Observed, which
// clears `removing` when a newer sequence proves the run is still live — so a
// marker still flagged here has not been contradicted.
func (p *Projector) applyRemoval(ctx context.Context, marker intake.Marker) error {
	if err := p.RemoveRun(ctx, marker.RunID); err != nil {
		return err
	}
	p.bump(func(s *Stats) { s.Removed++ })
	if err := p.intake.AckRemoval(ctx, marker.RunID); err != nil {
		p.bump(func(s *Stats) { s.AckFailures++ })
		p.options.Logger.Warn("removal acknowledgement failed",
			"run_id", marker.RunID, "error", err)
	}
	return nil
}

// prepare reads a run's journal and builds its projection.
//
// This is the expensive part, and it is what runs concurrently. It touches only
// the filesystem and pure functions — no read.db handle — so widening it cannot
// affect commit order.
func (p *Projector) prepare(ctx context.Context, runID string) (Projection, bool, error) {
	if err := ctx.Err(); err != nil {
		return Projection{}, false, err
	}
	if p.prepareForTest != nil {
		return p.prepareForTest(ctx, runID)
	}
	dir, found, err := p.locate(runID)
	if err != nil {
		return Projection{}, false, err
	}
	if !found {
		return Projection{}, false, nil
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Projection{}, false, nil
		}
		return Projection{}, false, fmt.Errorf("projector: read identity for %s: %w", runID, err)
	}
	identity, err := reader.Identity()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Projection{}, false, nil
		}
		return Projection{}, false, fmt.Errorf("projector: read identity for %s: %w", runID, err)
	}
	events, err := reader.Events()
	if err != nil {
		// A journal that exists but cannot be read IS worth surfacing: that is
		// corruption rather than absence, and silently skipping would make the
		// read model quietly incomplete.
		return Projection{}, false, fmt.Errorf("projector: read events for %s: %w", runID, err)
	}
	projection, err := readmodel.ProjectRunFromJournal(reader, identity, events)
	if err != nil {
		return Projection{}, false, fmt.Errorf("projector: project operator facts for %s: %w", runID, err)
	}
	return projection, true, nil
}

// locate finds a run's directory across the configured roots.
func (p *Projector) locate(runID string) (string, bool, error) {
	for _, root := range p.options.RunsDirs {
		candidate := filepath.Join(root, runID)
		info, err := os.Stat(candidate)
		if err == nil {
			if !info.IsDir() {
				return "", false, fmt.Errorf("projector: locate journal for %s: %s is not a directory", runID, candidate)
			}
			_, err = os.Stat(filepath.Join(candidate, "run.yaml"))
			if err == nil {
				return candidate, true, nil
			}
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", false, fmt.Errorf("projector: locate journal for %s: %w", runID, err)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("projector: locate journal for %s: %w", runID, err)
		}
	}
	return "", false, nil
}

// Restart performs the bounded startup pass: drain intake, then reproject the
// runs the read model records as non-terminal.
//
// # Why this is O(active + pending) rather than a scan
//
// Only two categories of run can have changed while the projector was not
// running: one that a writer reported (a pending intake marker), and one that
// was still in flight when we stopped (a non-terminal row). A terminal run
// cannot advance — that is what terminal means — so re-reading it would be work
// with a known-empty result.
//
// On the live instance that is tens of rows against 40,665 directories. A scan
// would be correct and useless: it would make startup cost proportional to
// history, which is the property §6.1 exists to remove.
func (p *Projector) Restart(ctx context.Context) (RestartResult, error) {
	var result RestartResult

	drained, err := p.Drain(ctx)
	if err != nil {
		return result, err
	}
	result.Drained = drained

	nonTerminal, err := p.store.NonTerminalRuns(ctx, restartReprojectLimit)
	if err != nil {
		return result, err
	}
	for _, row := range nonTerminal {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		projection, found, err := p.prepare(ctx, row.RunID)
		if err != nil {
			p.options.Logger.Warn("restart reprojection failed",
				"run_id", row.RunID, "error", err)
			p.bump(func(s *Stats) { s.ProjectFailures++ })
			continue
		}
		if !found {
			// Non-terminal in the read model, absent on disk. Repair's
			// bidirectional sweep owns this — it can distinguish a deliberate
			// removal from a lost one, and this pass cannot.
			result.Missing++
			continue
		}
		if err := p.UpsertRun(ctx, projection); err != nil {
			return result, err
		}
		result.Reprojected++
	}
	return result, nil
}

// RestartResult reports what the startup pass covered.
type RestartResult struct {
	Drained     int
	Reprojected int
	// Missing counts runs the read model holds as non-terminal but which have no
	// readable journal. Reported rather than resolved: the bidirectional repair
	// sweep (#1924) owns that decision.
	Missing int
}

// Stats returns a snapshot of the counters.
func (p *Projector) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

func (p *Projector) bump(mutate func(*Stats)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mutate(&p.stats)
}

func (p *Projector) recordDrain() {
	p.bump(func(s *Stats) { s.LastDrainAt = p.options.Now().UTC() })
}
