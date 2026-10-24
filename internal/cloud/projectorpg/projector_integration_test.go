//go:build cloudintegration

package projectorpg_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/projectorpg"
	"github.com/goobers/goobers/internal/cloud/readmodelpg"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/workflow"
)

type fixture struct {
	admin      *pgxpool.Pool
	controller *pgxpool.Pool
	api        *pgxpool.Pool
	backend    *journalpg.Backend
	writer     *livejournal.Writer
	projector  *lease.Manager
	wake       chan struct{}
}

func asRole(t *testing.T, admin *pgxpool.Pool, role string) *pgxpool.Pool {
	t.Helper()
	config := admin.Config().Copy()
	config.MaxConns = lease.MaxFencedConnections
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+role)
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	admin := pgtest.Pool(t, "projector")
	f := &fixture{admin: admin, controller: asRole(t, admin, "goobers_controller"), api: asRole(t, admin, "goobers_api"), wake: make(chan struct{}, 1)}
	controllerLease, err := lease.New(lease.Config{Pool: f.controller, InstanceID: pgtest.InstanceID, Name: lease.ControllerName, Holder: "journal", TTL: 2 * time.Minute, SelfFenceAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	handle := pgtest.Acquire(t, controllerLease)
	t.Cleanup(func() { _ = controllerLease.Release(context.Background(), handle) })
	f.backend, err = journalpg.New(journalpg.Config{Pool: f.controller, InstanceID: pgtest.InstanceID, Lease: controllerLease, LiveOwner: func() *lease.Handle { return handle }})
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.writer, err = livejournal.NewWriter(func(string) (string, bool) { return "/nonexistent/projector-test", true },
		livejournal.WithBackend(f.backend), livejournal.WithSpanSource(blobs),
		livejournal.WithContextObserver(func(context.Context, string, uint64) {
			select {
			case f.wake <- struct{}{}:
			default:
			}
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.writer.Close)
	f.projector, err = lease.New(lease.Config{Pool: f.controller, InstanceID: pgtest.InstanceID, Name: projectorpg.LeaseName, Holder: "projector", TTL: 2 * time.Minute, SelfFenceAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func runID(c byte) string { return strings.Repeat(string(c), 32) }

// emit appends events to a run: the first call opens it.
func (f *fixture) emit(t *testing.T, id string, open bool, events ...journal.Event) {
	t.Helper()
	f.emitWithGraphDigest(t, id, open, "sha256:abc", events...)
}

// emitWithGraphDigest pins a graph digest that may disagree with the run
// identity's, which the projection refuses: an unprojectable run.
func (f *fixture) emitWithGraphDigest(t *testing.T, id string, open bool, graphDigest string, events ...journal.Event) {
	t.Helper()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	graph, err := json.Marshal(workflow.Graph{Name: "wf", Version: 1, Digest: graphDigest})
	if err != nil {
		t.Fatal(err)
	}
	request := livejournal.EmitRequest{RunID: id, Gaggle: "web"}
	if open {
		request.Open = &livejournal.OpenHeader{
			Identity: journal.RunIdentity{RunID: id, Workflow: "wf", WorkflowVersion: 1, WorkflowDigest: "sha256:abc", Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}},
			Graph:    graph, Definition: []byte(`{"name":"wf"}`),
		}
		started := journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)}
		request.Ops = append(request.Ops, livejournal.Op{Kind: livejournal.OpAppend, Key: id + "|start", Event: &started, Time: at})
	}
	for i, event := range events {
		event := event
		request.Ops = append(request.Ops, livejournal.Op{Kind: livejournal.OpAppend, Key: id + "|" + string(event.Type) + "|" + event.Stage, Event: &event, Time: at.Add(time.Duration(i+1) * time.Second)})
	}
	if _, err := f.writer.Emit(t.Context(), request); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) runner(t *testing.T, batch int, health *projectorpg.Health, report func(error)) (*projectorpg.Runner, *readmodelpg.ActiveReader) {
	t.Helper()
	handle := pgtest.Acquire(t, f.projector)
	t.Cleanup(func() { _ = f.projector.Release(context.Background(), handle) })
	return f.runnerAs(t, f.projector, handle, batch, health, report)
}

// runnerAs builds a runner for an already-acquired projector lease handle.
func (f *fixture) runnerAs(t *testing.T, manager *lease.Manager, handle *lease.Handle, batch int, health *projectorpg.Health, report func(error)) (*projectorpg.Runner, *readmodelpg.ActiveReader) {
	t.Helper()
	projector, err := readmodelpg.NewProjector(t.Context(), f.controller, manager, handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := projectorpg.New(projectorpg.Config{Projector: projector, Journal: f.backend, InstanceID: pgtest.InstanceID, Batch: batch, Report: report, Health: health})
	if err != nil {
		t.Fatal(err)
	}
	// The API login role reads the model: the read side must work for both roles.
	reader, err := readmodelpg.NewActiveReader(t.Context(), f.api)
	if err != nil {
		t.Fatal(err)
	}
	return runner, reader
}

func TestSyncProjectsJournalHistoryAsTheControllerAndTheAPIRoleReadsIt(t *testing.T) {
	f := newFixture(t)
	runner, reader := f.runner(t, 0, nil, func(err error) { t.Errorf("projection failure: %v", err) })
	id := runID('a')
	f.emit(t, id, true, journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1})

	if n, err := runner.Sync(t.Context()); err != nil || n != 1 {
		t.Fatalf("first sync projected %d (%v), want 1", n, err)
	}
	row, ok, err := reader.GetRun(t.Context(), id)
	if err != nil || !ok || row.Phase != journal.PhaseRunning || row.LastSeq != 2 || row.Gaggle != "web" || row.Workflow != "wf" {
		t.Fatalf("projected row = %+v ok=%v err=%v", row, ok, err)
	}
	if n, err := runner.Sync(t.Context()); err != nil || n != 0 {
		t.Fatalf("unchanged journal re-projected %d (%v), want 0", n, err)
	}

	f.emit(t, id, false, journal.Event{Type: journal.EventStageFinished, Stage: "build", Attempt: 1, Status: "success"})
	if n, err := runner.Sync(t.Context()); err != nil || n != 1 {
		t.Fatalf("advanced journal projected %d (%v), want 1", n, err)
	}
	row, _, err = reader.GetRun(t.Context(), id)
	if err != nil || row.LastSeq != 3 {
		t.Fatalf("row after advance = %+v (%v), want LastSeq 3", row, err)
	}
	var watermark int64
	if err := f.admin.QueryRow(t.Context(), `SELECT source_seq FROM readmodel_v1.source_watermark WHERE run_id=$1`, id).Scan(&watermark); err != nil || watermark != 3 {
		t.Fatalf("source watermark = %d (%v), want 3", watermark, err)
	}
}

func TestCatchUpAfterARestartPagesThroughEveryStaleRun(t *testing.T) {
	f := newFixture(t)
	ids := []string{runID('a'), runID('b'), runID('c')}
	for _, id := range ids {
		f.emit(t, id, true)
	}
	// A fresh runner stands for a restarted process: nothing was notified, and the
	// page size of one forces the cursor to carry across pages.
	runner, reader := f.runner(t, 1, nil, func(err error) { t.Errorf("projection failure: %v", err) })
	if n, err := runner.Sync(t.Context()); err != nil || n != len(ids) {
		t.Fatalf("catch-up projected %d (%v), want %d", n, err, len(ids))
	}
	for _, id := range ids {
		if _, ok, err := reader.GetRun(t.Context(), id); err != nil || !ok {
			t.Fatalf("run %s missing after catch-up: ok=%v err=%v", id, ok, err)
		}
	}
}

func TestServeProjectsUnderTheProjectorLeaseAndReleasesItOnShutdown(t *testing.T) {
	f := newFixture(t)
	reader, err := readmodelpg.NewActiveReader(t.Context(), f.api)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	var reported []error
	done := make(chan error, 1)
	go func() {
		done <- projectorpg.Serve(ctx, projectorpg.ServeConfig{
			Manager: f.projector, Pool: f.controller, Journal: f.backend, InstanceID: pgtest.InstanceID, Wake: f.wake, Retry: 50 * time.Millisecond,
			Report: func(err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() },
		})
	}()
	id := runID('d')
	f.emit(t, id, true)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok, err := reader.GetRun(t.Context(), id); err == nil && ok {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("the served projector never projected the run; reported=%v", reported)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var released bool
	if err := f.admin.QueryRow(t.Context(), `SELECT released_at IS NOT NULL FROM control.lease_history WHERE instance_id=$1 AND name=$2 ORDER BY epoch DESC LIMIT 1`, pgtest.InstanceID, projectorpg.LeaseName).Scan(&released); err != nil || !released {
		t.Fatalf("projector lease released=%v (%v) after shutdown", released, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 0 {
		t.Fatalf("clean run reported %v", reported)
	}
}

func TestProjectorWritesAreRefusedToTheAPIRole(t *testing.T) {
	f := newFixture(t)
	for name, statement := range map[string]string{
		"read model write":    `UPDATE readmodel_v1.source_watermark SET source_seq = 1`,
		"watermark read":      `SELECT 1 FROM readmodel_v1.source_watermark`,
		"journal counters":    `SELECT 1 FROM journal.run_seq`,
		"journal events":      `SELECT 1 FROM journal.run_event`,
		"journal delete":      `DELETE FROM journal.run_event`,
		"read model deletion": `DELETE FROM readmodel_v1.run`,
	} {
		_, err := f.api.Exec(t.Context(), statement)
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Code != "42501" {
			t.Fatalf("API role %s = %v, want insufficient_privilege (42501)", name, err)
		}
	}
}

func TestHealthReportsOpenGapsOncePerDistinctFailureAndClearsThemWhenRepaired(t *testing.T) {
	f := newFixture(t)
	var reported []error
	health := &projectorpg.Health{}
	runner, _ := f.runner(t, 0, health, func(err error) { reported = append(reported, err) })
	if snap := health.Snapshot(); snap.ApplyFailures != 0 || !snap.LastDrainAt.IsZero() {
		t.Fatalf("health before any pass = %+v", snap)
	}
	good, bad := runID('a'), runID('b')
	f.emit(t, good, true)
	f.emitWithGraphDigest(t, bad, true, "sha256:other")
	for range 3 {
		if _, err := runner.Sync(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	snap := health.Snapshot()
	if snap.ApplyFailures != 1 || snap.LastDrainAt.IsZero() {
		t.Fatalf("health with one unprojectable run = %+v, want one open gap and a completed pass", snap)
	}
	if len(reported) != 1 {
		t.Fatalf("reported %d times over 3 passes, want once per distinct failure: %v", len(reported), reported)
	}
}

// takeover expires the projector heartbeat on the database clock and acquires
// the lease for a foreign holder at the next epoch.
func (f *fixture) takeover(t *testing.T, holder string) (*lease.Manager, *lease.Handle) {
	t.Helper()
	if _, err := f.admin.Exec(t.Context(), `UPDATE control.lease_heartbeat SET expires_at=now()-interval '1 second' WHERE instance_id=$1 AND name=$2`, pgtest.InstanceID, projectorpg.LeaseName); err != nil {
		t.Fatal(err)
	}
	successor, err := lease.New(lease.Config{Pool: f.controller, InstanceID: pgtest.InstanceID, Name: projectorpg.LeaseName, Holder: holder, TTL: 2 * time.Minute, SelfFenceAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := successor.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = successor.Release(context.Background(), handle) })
	return successor, handle
}

func TestLeaseTakeoverEndsTheOldEpochCommitsNothingAndTheNewHolderProjects(t *testing.T) {
	f := newFixture(t)
	var reported []error
	old, reader := f.runner(t, 0, nil, func(err error) { reported = append(reported, err) })
	first := runID('a')
	f.emit(t, first, true)
	if n, err := old.Sync(t.Context()); err != nil || n != 1 {
		t.Fatalf("holder projected %d (%v), want 1", n, err)
	}
	successor, successorHandle := f.takeover(t, "successor")
	if successorHandle.Epoch < 2 {
		t.Fatalf("successor epoch = %d, want a higher epoch", successorHandle.Epoch)
	}
	second := runID('b')
	f.emit(t, second, true)

	// The deposed epoch must stop with the authority error, not swallow it as a
	// per-run failure and keep looping; and it must have committed nothing.
	if n, err := old.Sync(t.Context()); !errors.Is(err, lease.ErrStale) || n != 0 {
		t.Fatalf("deposed holder Sync = %d, %v; want lease.ErrStale and no projection", n, err)
	}
	if len(reported) != 0 {
		t.Fatalf("authority loss was reported as a per-run failure: %v", reported)
	}
	if _, ok, err := reader.GetRun(t.Context(), second); err != nil || ok {
		t.Fatalf("the deposed holder committed run %s: ok=%v err=%v", second, ok, err)
	}

	next, _ := f.runnerAs(t, successor, successorHandle, 0, nil, func(err error) { t.Errorf("successor failure: %v", err) })
	if n, err := next.Sync(t.Context()); err != nil || n != 1 {
		t.Fatalf("new holder projected %d (%v), want the one new run", n, err)
	}
	if _, ok, err := reader.GetRun(t.Context(), second); err != nil || !ok {
		t.Fatalf("new holder did not project run %s: ok=%v err=%v", second, ok, err)
	}
}

func TestAFailingRunAtAPageBoundaryDoesNotPinTheCatchUp(t *testing.T) {
	f := newFixture(t)
	good1, bad, good2 := runID('a'), runID('b'), runID('c')
	f.emit(t, good1, true)
	f.emitWithGraphDigest(t, bad, true, "sha256:other")
	f.emit(t, good2, true)
	var reported []error
	runner, reader := f.runner(t, 1, nil, func(err error) { reported = append(reported, err) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// Page size one puts the failing run alone on a page boundary: the cursor
	// must advance past it, or this never returns.
	n, err := runner.Sync(ctx)
	if err != nil || n != 2 {
		t.Fatalf("catch-up projected %d (%v), want the two good runs", n, err)
	}
	for _, id := range []string{good1, good2} {
		if _, ok, err := reader.GetRun(t.Context(), id); err != nil || !ok {
			t.Fatalf("good run %s missing: ok=%v err=%v", id, ok, err)
		}
	}
	if _, ok, _ := reader.GetRun(t.Context(), bad); ok || len(reported) != 1 {
		t.Fatalf("failing run: projected=%v reports=%d, want it skipped and reported once", ok, len(reported))
	}
}

func TestServeStandbyRetriesWithoutReportingContentionAndTakesOverWhenReleased(t *testing.T) {
	f := newFixture(t)
	holder := pgtest.Acquire(t, f.projector) // the foreign replica holds the lease
	reader, err := readmodelpg.NewActiveReader(t.Context(), f.api)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	var reported []error
	done := make(chan error, 1)
	standby, err := lease.New(lease.Config{Pool: f.controller, InstanceID: pgtest.InstanceID, Name: projectorpg.LeaseName, Holder: "standby", TTL: 2 * time.Minute, SelfFenceAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		done <- projectorpg.Serve(ctx, projectorpg.ServeConfig{
			Manager: standby, Pool: f.controller, Journal: f.backend, InstanceID: pgtest.InstanceID, Wake: f.wake, Retry: 20 * time.Millisecond,
			Report: func(err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() },
		})
	}()
	id := runID('e')
	f.emit(t, id, true)
	time.Sleep(500 * time.Millisecond) // many standby attempts at a 20ms retry
	if _, ok, err := reader.GetRun(t.Context(), id); err != nil || ok {
		t.Fatalf("a standby projected while another replica held the lease: ok=%v err=%v", ok, err)
	}
	mu.Lock()
	contention := append([]error(nil), reported...)
	mu.Unlock()
	if len(contention) != 0 {
		t.Fatalf("lease contention was reported as a failure (and would back off): %v", contention)
	}
	if err := f.projector.Release(t.Context(), holder); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, ok, err := reader.GetRun(t.Context(), id); err == nil && ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the standby never took over after the holder released")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
