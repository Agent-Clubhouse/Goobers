//go:build cloudintegration

package projectorpg_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	graph, err := json.Marshal(workflow.Graph{Name: "wf", Version: 1, Digest: "sha256:abc"})
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

func (f *fixture) runner(t *testing.T, batch int) (*projectorpg.Runner, *readmodelpg.ActiveReader) {
	t.Helper()
	handle := pgtest.Acquire(t, f.projector)
	t.Cleanup(func() { _ = f.projector.Release(context.Background(), handle) })
	projector, err := readmodelpg.NewProjector(t.Context(), f.controller, f.projector, handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := projectorpg.New(projectorpg.Config{Projector: projector, Journal: f.backend, InstanceID: pgtest.InstanceID, Batch: batch, Report: func(err error) { t.Errorf("projection failure: %v", err) }})
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
	runner, reader := f.runner(t, 0)
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
	runner, reader := f.runner(t, 1)
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
	if _, err := f.api.Exec(t.Context(), `UPDATE readmodel_v1.source_watermark SET source_seq = 1`); err == nil {
		t.Fatal("the API role obtained a read model write")
	}
	if _, err := f.api.Exec(t.Context(), `SELECT 1 FROM journal.run_seq`); err == nil {
		t.Fatal("the API role read the journal counters the projector depends on")
	}
}
