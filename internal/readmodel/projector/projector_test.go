package projector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readmodel/repair"
)

// fakeStore records commit order, which is the property most of these tests are
// about. A real store would too, but through a change table that makes the
// assertion indirect; here the order is the observation.
type fakeStore struct {
	mu sync.Mutex
	// commits is the order in which writes were APPLIED, which is what change
	// sequence allocation follows.
	commits []string
	rows    map[string]readmodel.RunRow
	// hold, if set, blocks inside UpsertRun until released — used to prove the
	// commit loop is serialized rather than merely usually-ordered.
	hold      chan struct{}
	holdFor   string
	upsertErr error
	// concurrent tracks the maximum number of simultaneous commits observed.
	inFlight    int32
	maxInFlight int32
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]readmodel.RunRow{}}
}

func (f *fakeStore) UpsertRun(_ context.Context, p Projection) error {
	current := atomic.AddInt32(&f.inFlight, 1)
	for {
		observed := atomic.LoadInt32(&f.maxInFlight)
		if current <= observed || atomic.CompareAndSwapInt32(&f.maxInFlight, observed, current) {
			break
		}
	}
	defer atomic.AddInt32(&f.inFlight, -1)

	if f.hold != nil && p.Run.RunID == f.holdFor {
		<-f.hold
	}
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, p.Run.RunID)
	f.rows[p.Run.RunID] = p.Run
	return nil
}

func (f *fakeStore) GetRun(_ context.Context, runID string) (readmodel.RunRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[runID]
	return row, ok, nil
}

func (f *fakeStore) NonTerminalRuns(_ context.Context, limit int) ([]readmodel.RunRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []readmodel.RunRow
	for _, row := range f.rows {
		if !row.Terminal && len(out) < limit {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeStore) RemoveRun(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, "remove:"+runID)
	delete(f.rows, runID)
	return nil
}

func (f *fakeStore) SaveSweepCursor(_ context.Context, _ readmodel.SweepCursor) error {
	current := atomic.AddInt32(&f.inFlight, 1)
	for {
		observed := atomic.LoadInt32(&f.maxInFlight)
		if current <= observed || atomic.CompareAndSwapInt32(&f.maxInFlight, observed, current) {
			break
		}
	}
	defer atomic.AddInt32(&f.inFlight, -1)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, "sweep-cursor")
	return nil
}

func (f *fakeStore) MarkUnpublished(
	_ context.Context,
	runID string,
	_ time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, "mark-unpublished:"+runID)
	return nil
}

func (f *fakeStore) ClearUnpublished(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, "clear-unpublished:"+runID)
	return nil
}

func (f *fakeStore) Tombstone(
	_ context.Context,
	runID string,
	_ time.Time,
	_ string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, "tombstone:"+runID)
	return nil
}

func (f *fakeStore) SetProjectionFloor(_ context.Context, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, "projection-floor")
	return nil
}

func (f *fakeStore) PruneChangeFeed(_ context.Context, _ int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, "prune-change-feed")
	return 0, nil
}

func (f *fakeStore) SweepCursor(context.Context) (readmodel.SweepCursor, error) {
	return readmodel.SweepCursor{}, nil
}

func (f *fakeStore) ProjectionFloor(context.Context) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (f *fakeStore) IsUnpublished(context.Context, string, time.Time) (bool, error) {
	return false, nil
}

func (f *fakeStore) Tombstoned(context.Context, string) (bool, error) {
	return false, nil
}

func (f *fakeStore) ProjectedRunIDsAfter(
	context.Context,
	time.Time,
	string,
	time.Time,
	int,
) ([]readmodel.RunRow, error) {
	return nil, nil
}

func (f *fakeStore) commitOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commits...)
}

// TestRepairMutationsShareTheProjectionCommitLoop prevents repair from becoming
// a second read-model writer.
func TestRepairMutationsShareTheProjectionCommitLoop(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.hold = make(chan struct{})
	store.holdFor = "run-a"

	p := New(store, newFakeIntake(), Options{})
	stop := p.Start(ctx)
	defer stop()

	upserted := make(chan error, 1)
	go func() { upserted <- p.UpsertRun(ctx, projectionFor("run-a", 1)) }()
	waitFor(t, func() bool { return atomic.LoadInt32(&store.inFlight) == 1 })

	swept := make(chan error, 1)
	sweeper := repair.New(store, p, nil, repair.Options{
		RunsDirs:  []string{t.TempDir()},
		BatchSize: 1,
	})
	go func() { swept <- sweeper.Step(ctx) }()

	select {
	case err := <-swept:
		t.Fatalf("repair completed while projection commit was blocked: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(store.hold)

	if err := <-upserted; err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := <-swept; err != nil {
		t.Fatalf("repair sweep: %v", err)
	}
	if got := atomic.LoadInt32(&store.maxInFlight); got != 1 {
		t.Errorf("observed %d simultaneous projector and repair writes; repair bypassed "+
			"the sole-writer commit loop", got)
	}
	if got := store.commitOrder(); len(got) != 2 ||
		got[0] != "run-a" || got[1] != "sweep-cursor" {
		t.Errorf("commit order = %v, want [run-a sweep-cursor]", got)
	}
}

func TestAcceptedCommitFinishesBeforeCancellationReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	projector := New(newFakeStore(), newFakeIntake(), Options{})
	stop := projector.Start(context.Background())
	defer stop()

	accepted := make(chan struct{})
	release := make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		committed <- projector.commit(ctx, commitRequest{write: func(context.Context, Store) error {
			close(accepted)
			<-release
			return nil
		}})
	}()

	<-accepted
	cancel()
	select {
	case err := <-committed:
		t.Fatalf("accepted commit returned before its write finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-committed; err != nil {
		t.Fatalf("accepted commit: %v", err)
	}
}

// TestCommitsAreSerializedUnderConcurrentPreparation is #1923's third acceptance
// criterion: "no client can be stranded past a lower uncommitted seq — asserted
// under concurrent preparation."
//
// The failure this prevents: two workers allocate change sequences 10 and 11; 11
// commits first; a client advances its cursor past 11; then 10 commits, and
// `WHERE seq > 11` never returns it. The client has missed a transition and
// cannot detect that it did.
//
// The assertion is that at most ONE commit is ever in flight, which is the
// property that makes sequence allocation follow commit order. Asserting the
// resulting order alone would be weaker — a run of correct orderings can happen
// by luck on an unloaded machine.
func TestCommitsAreSerializedUnderConcurrentPreparation(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	watermarks := newFakeIntake()

	const runs = 24
	for i := 0; i < runs; i++ {
		watermarks.observe(fmt.Sprintf("run-%02d", i), uint64(i+1))
	}

	projector := New(store, watermarks, Options{Workers: 8, DrainLimit: runs})
	stop := projector.Start(ctx)
	defer stop()

	// prepare is stubbed so the test exercises the commit path without needing
	// real journals on disk; the concurrency it is proving is in the commit
	// loop, not in journal reading.
	projector.prepareForTest = func(_ context.Context, runID string) (Projection, bool, error) {
		return projectionFor(runID, watermarks.seqOf(runID)), true, nil
	}

	if _, err := projector.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if got := atomic.LoadInt32(&store.maxInFlight); got != 1 {
		t.Errorf("observed %d simultaneous commits with 8 workers; the commit loop is not "+
			"serialized, so two workers can allocate change sequences out of commit order "+
			"and strand a client past a lower uncommitted one", got)
	}
	if len(store.commitOrder()) != runs {
		t.Errorf("committed %d runs, want %d", len(store.commitOrder()), runs)
	}
}

// TestAckHappensAfterCommitNotBefore pins the ordering the whole intake protocol
// rests on.
//
// The acknowledgement cannot be inside the projection transaction (separate
// databases, no atomic cross-database commit), so the protocol is: commit, THEN
// acknowledge. Reversed, a crash between them loses the work entirely — the
// marker is gone and nothing was written.
func TestAckHappensAfterCommitNotBefore(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	watermarks := newFakeIntake()
	watermarks.observe("run-a", 5)

	// Block the commit. If the ack were issued first, it would land while the
	// commit is still held.
	store.hold = make(chan struct{})
	store.holdFor = "run-a"

	projector := New(store, watermarks, Options{Workers: 1, DrainLimit: 4})
	stop := projector.Start(ctx)
	defer stop()
	projector.prepareForTest = func(_ context.Context, runID string) (Projection, bool, error) {
		return projectionFor(runID, 5), true, nil
	}

	drained := make(chan error, 1)
	go func() { _, err := projector.Drain(ctx); drained <- err }()

	// While the commit is held, nothing may have been acknowledged.
	waitFor(t, func() bool { return atomic.LoadInt32(&store.inFlight) == 1 })
	if got := watermarks.ackCount(); got != 0 {
		t.Errorf("%d acknowledgements were issued while the projection was still "+
			"uncommitted; a crash here would lose the work with no marker left to "+
			"rediscover it", got)
	}

	close(store.hold)
	if err := <-drained; err != nil {
		t.Fatalf("drain: %v", err)
	}
	if watermarks.ackCount() != 1 {
		t.Errorf("after commit, %d acknowledgements, want 1", watermarks.ackCount())
	}
}

// TestAckFailureIsDegradationNotLoss pins that a failed acknowledgement leaves
// the projection in place and the marker pending.
//
// The projection already committed. Treating the ack failure as an error for the
// run would discard correct work; leaving the marker means the next pass
// reprojects idempotently and re-acknowledges.
func TestAckFailureIsDegradationNotLoss(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	watermarks := newFakeIntake()
	watermarks.observe("run-a", 5)
	watermarks.ackErr = errors.New("intake unavailable")

	projector := New(store, watermarks, Options{Workers: 1})
	stop := projector.Start(ctx)
	defer stop()
	projector.prepareForTest = func(_ context.Context, runID string) (Projection, bool, error) {
		return projectionFor(runID, 5), true, nil
	}

	if _, err := projector.Drain(ctx); err != nil {
		t.Fatalf("a failed acknowledgement must not fail the drain: %v", err)
	}
	if _, ok, _ := store.GetRun(ctx, "run-a"); !ok {
		t.Error("the projection was rolled back because the acknowledgement failed; correct " +
			"work was discarded for an unrelated reason")
	}
	if projector.Stats().AckFailures != 1 {
		t.Errorf("ack failures = %d, want 1; the degraded condition must be countable",
			projector.Stats().AckFailures)
	}
	if len(watermarks.pending()) != 1 {
		t.Error("the marker was consumed despite the acknowledgement failing")
	}
}

// TestOneRunsFailureDoesNotAbortThePass pins batch isolation.
//
// The read model is derived, so a run that fails to project is rediscovered by
// the repair sweep. Aborting the pass would leave every LATER marker unprocessed
// for a reason that has nothing to do with them — turning one bad journal into a
// stalled read model.
func TestOneRunsFailureDoesNotAbortThePass(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	watermarks := newFakeIntake()
	for _, id := range []string{"run-a", "run-bad", "run-c"} {
		watermarks.observe(id, 1)
	}

	projector := New(store, watermarks, Options{Workers: 1, DrainLimit: 10})
	stop := projector.Start(ctx)
	defer stop()
	projector.prepareForTest = func(_ context.Context, runID string) (Projection, bool, error) {
		if runID == "run-bad" {
			return Projection{}, false, errors.New("corrupt journal")
		}
		return projectionFor(runID, 1), true, nil
	}

	handled, err := projector.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if handled != 2 {
		t.Errorf("handled %d runs, want 2 (the two good ones)", handled)
	}
	for _, want := range []string{"run-a", "run-c"} {
		if _, ok, _ := store.GetRun(ctx, want); !ok {
			t.Errorf("%s was not projected; one corrupt journal stalled unrelated runs", want)
		}
	}
	if projector.Stats().ProjectFailures != 1 {
		t.Errorf("project failures = %d, want 1", projector.Stats().ProjectFailures)
	}
}

// TestRestartIsBoundedByActiveAndPending is #1923's second acceptance criterion.
//
// Only two categories of run can have changed while the projector was down: one
// a writer reported (a pending marker) and one still in flight when we stopped (a
// non-terminal row). A terminal run cannot advance — that is what terminal means
// — so re-reading it is work with a known-empty result.
//
// The assertion counts journal preparations, not time: on the live instance this
// is the difference between tens of rows and 40,665 directories.
func TestRestartIsBoundedByActiveAndPending(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	watermarks := newFakeIntake()

	// 200 terminal runs already projected — history that cannot have changed.
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("done-%03d", i)
		store.rows[id] = readmodel.RunRow{
			RunID: id, Gaggle: "alpha", Workflow: "wf",
			Phase: journal.PhaseCompleted, Terminal: true, LastSeq: 9,
		}
	}
	// Three still in flight, and two pending markers.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("live-%d", i)
		store.rows[id] = readmodel.RunRow{
			RunID: id, Gaggle: "alpha", Workflow: "wf",
			Phase: journal.PhaseRunning, Terminal: false, LastSeq: 2,
		}
	}
	watermarks.observe("pending-0", 1)
	watermarks.observe("pending-1", 1)

	projector := New(store, watermarks, Options{Workers: 2, DrainLimit: 50})
	stop := projector.Start(ctx)
	defer stop()

	var prepared int32
	projector.prepareForTest = func(_ context.Context, runID string) (Projection, bool, error) {
		atomic.AddInt32(&prepared, 1)
		return projectionFor(runID, 3), true, nil
	}

	result, err := projector.Restart(ctx)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}

	const wantPrepared = 5 // 2 pending + 3 non-terminal
	if got := atomic.LoadInt32(&prepared); got != wantPrepared {
		t.Errorf("restart prepared %d runs, want %d (pending + non-terminal). It is reading "+
			"terminal history, which cannot have advanced — that makes startup cost "+
			"proportional to total runs rather than to active ones", got, wantPrepared)
	}
	if result.Drained != 2 || result.Reprojected != 3 {
		t.Errorf("restart result = %+v, want 2 drained and 3 reprojected", result)
	}
}

// TestRemovalTakesTheRemovalBranchAndConfirms pins intent → project → confirm.
func TestRemovalTakesTheRemovalBranchAndConfirms(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.rows["run-gone"] = readmodel.RunRow{RunID: "run-gone", Terminal: true}
	watermarks := newFakeIntake()
	watermarks.markRemoving("run-gone")

	projector := New(store, watermarks, Options{Workers: 1})
	stop := projector.Start(ctx)
	defer stop()

	if _, err := projector.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, ok, _ := store.GetRun(ctx, "run-gone"); ok {
		t.Error("a run with pending removal intent was not removed")
	}
	if len(watermarks.pending()) != 0 {
		t.Error("the removal marker was not confirmed after the removal committed")
	}
	if projector.Stats().Removed != 1 {
		t.Errorf("removed = %d, want 1", projector.Stats().Removed)
	}
}

// TestWatermarkWithNoJournalLeavesTheMarker pins that an unreadable run is
// deferred to repair rather than silently acknowledged.
//
// A marker with no journal behind it is ambiguous: the journal may have been
// removed with its removal intent lost, or may not have been written yet.
// Consuming the marker here would erase the only record that anything was
// expected; the repair sweep can see the whole picture and decide.
func TestWatermarkWithNoJournalLeavesTheMarker(t *testing.T) {
	for _, tc := range []struct {
		name      string
		createDir bool
	}{
		{name: "run directory disappeared"},
		{name: "journal disappeared", createDir: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newFakeStore()
			watermarks := newFakeIntake()
			watermarks.observe("run-absent", 4)
			runsDir := t.TempDir()
			if tc.createDir {
				if err := os.Mkdir(filepath.Join(runsDir, "run-absent"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			projector := New(store, watermarks, Options{
				Workers: 1, RunsDirs: []string{runsDir},
			})
			stop := projector.Start(ctx)
			defer stop()

			if _, err := projector.Drain(ctx); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if len(watermarks.pending()) != 1 {
				t.Error("a marker with no journal was acknowledged away; the only record " +
					"that the run was expected is now gone")
			}
			if watermarks.ackCount() != 0 {
				t.Errorf("%d acknowledgements issued for a disappeared journal", watermarks.ackCount())
			}
			if projector.Stats().ProjectFailures != 0 {
				t.Errorf("disappeared journal counted as a projection failure")
			}
		})
	}
}

func TestJournalIdentityFailuresRemainVisibleAndRetryable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "malformed",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("schema: ["), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsupported",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("schema: goobers.dev/journal/run/v2\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unreadable",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			const runID = "run-bad-identity"
			runsDir := t.TempDir()
			runDir := filepath.Join(runsDir, runID)
			if err := os.Mkdir(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, filepath.Join(runDir, "run.yaml"))

			watermarks := newFakeIntake()
			watermarks.observe(runID, 4)
			var logs bytes.Buffer
			drainedAt := time.Date(2026, 8, 5, 7, 0, 0, 0, time.UTC)
			projector := New(newFakeStore(), watermarks, Options{
				Workers: 1, RunsDirs: []string{runsDir},
				Logger: slog.New(slog.NewTextHandler(&logs, nil)),
				Now:    func() time.Time { return drainedAt },
			})
			stop := projector.Start(ctx)
			defer stop()

			handled, err := projector.Drain(ctx)
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			if handled != 0 {
				t.Errorf("handled = %d, want 0", handled)
			}
			if len(watermarks.pending()) != 1 || watermarks.ackCount() != 0 {
				t.Fatal("failed journal marker was not retained for retry")
			}
			stats := projector.Stats()
			if stats.ProjectFailures != 1 {
				t.Errorf("project failures = %d, want 1", stats.ProjectFailures)
			}
			if !stats.LastDrainAt.Equal(drainedAt) {
				t.Errorf("last drain at = %v, want %v", stats.LastDrainAt, drainedAt)
			}
			if log := logs.String(); !strings.Contains(log, "project run failed") ||
				!strings.Contains(log, "run_id="+runID) ||
				!strings.Contains(log, "read identity for "+runID) {
				t.Errorf("failure log = %q, want operation and run ID", log)
			}
		})
	}
}

// TestStopIsIdempotentAndDrainsCleanly pins that the returned stop function can
// be called more than once — a daemon with several shutdown paths must not panic
// on a double close.
func TestStopIsIdempotentAndDrainsCleanly(t *testing.T) {
	projector := New(newFakeStore(), newFakeIntake(), Options{Interval: time.Hour})
	stop := projector.Start(context.Background())
	stop()
	stop()
}

// projectionFor builds a minimal terminal projection.
func projectionFor(runID string, seq uint64) Projection {
	startedAt := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	finished := startedAt.Add(time.Minute)
	return Projection{Run: readmodel.RunRow{
		RunID: runID, Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: startedAt, FinishedAt: &finished, LastSeq: seq,
	}}
}

// waitFor polls a condition with a deadline, so a serialization test cannot hang
// the suite when it fails.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond) // Polling interval for the fake store's synchronized in-flight state.
	}
	t.Fatal("condition not met within 5s")
}

// fakeIntake is an in-memory watermark store implementing the projector's narrow
// Intake interface, with the same guards the real one has.
type fakeIntake struct {
	mu      sync.Mutex
	markers map[string]intake.Marker
	order   []string
	acks    int
	ackErr  error
}

func newFakeIntake() *fakeIntake {
	return &fakeIntake{markers: map[string]intake.Marker{}}
}

func (f *fakeIntake) observe(runID string, seq uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.markers[runID]; !exists {
		f.order = append(f.order, runID)
	}
	f.markers[runID] = intake.Marker{RunID: runID, SourceSeq: seq}
}

func (f *fakeIntake) markRemoving(runID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.markers[runID]; !exists {
		f.order = append(f.order, runID)
	}
	marker := f.markers[runID]
	marker.RunID = runID
	marker.Removing = true
	f.markers[runID] = marker
}

func (f *fakeIntake) seqOf(runID string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.markers[runID].SourceSeq
}

func (f *fakeIntake) Pending(_ context.Context, limit int) ([]intake.Marker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []intake.Marker
	for _, id := range f.order {
		marker, ok := f.markers[id]
		if !ok || len(out) >= limit {
			continue
		}
		out = append(out, marker)
	}
	return out, nil
}

func (f *fakeIntake) Ack(_ context.Context, runID string, projectedSeq uint64) (bool, error) {
	if f.ackErr != nil {
		return false, f.ackErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks++
	marker, ok := f.markers[runID]
	// The same guards the real store applies: a newer sequence survives, and a
	// removal marker is never consumed as progress.
	if !ok || marker.Removing || marker.SourceSeq > projectedSeq {
		return false, nil
	}
	delete(f.markers, runID)
	return true, nil
}

func (f *fakeIntake) AckRemoval(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.markers, runID)
	return nil
}

func (f *fakeIntake) pending() []intake.Marker {
	markers, _ := f.Pending(context.Background(), 1000)
	return markers
}

func (f *fakeIntake) ackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acks
}
