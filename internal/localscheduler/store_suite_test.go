package localscheduler_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type schedulerPolicyStores struct {
	claims    localscheduler.ClaimStore
	state     localscheduler.SchedulerState
	accept    triggerqueue.OrdinaryStore
	source    triggerqueue.SourceAcceptanceStore
	cursor    triggerqueue.ScheduleCursorStore
	journal   journal.InstanceStore
	reconcile localscheduler.RunDirReconciler
	close     func()
	dir       string
	runsDir   string
}

func fileSchedulerPolicyStores(t *testing.T) schedulerPolicyStores {
	t.Helper()
	root := t.TempDir()
	schedulerDir := filepath.Join(root, "scheduler")
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claims, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	queue, err := triggerqueue.Open(filepath.Join(schedulerDir, "accepted-triggers.db"))
	if err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		_ = queue.Close()
		t.Fatal(err)
	}
	return schedulerPolicyStores{
		claims: claims, state: localscheduler.NewFileSchedulerState(),
		accept: queue, source: queue, cursor: queue, journal: log,
		reconcile: localscheduler.NewFileRunDirReconciler(),
		close: func() {
			_ = log.Close()
			_ = queue.Close()
		},
		dir: schedulerDir, runsDir: filepath.Join(root, "runs"),
	}
}

func TestSchedulerPolicyStoreSuite(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		stores := fileSchedulerPolicyStores(t)
		t.Cleanup(stores.close)
		testClaimStore(t, stores.claims)
		testSchedulerState(t, stores.state, stores.dir)
		testAcceptanceStore(t, stores.accept)
		testScheduleCursor(t, stores.source, stores.cursor)
		testPendingLimit(t, stores.source)
		testInstanceStore(t, stores.journal)
		testRunDirReconciler(t, stores.reconcile, stores.runsDir)
	})
}

func testClaimStore(t *testing.T, store localscheduler.ClaimStore) {
	t.Helper()
	key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	ok, holder, err := store.ClaimScoped(key, "run-a", "implementation", time.Hour)
	if err != nil || !ok || holder != "run-a" {
		t.Fatalf("claim = %v, %q, %v", ok, holder, err)
	}
	ok, holder, err = store.ClaimScoped(key, "run-b", "implementation", time.Hour)
	if err != nil || ok || holder != "run-a" {
		t.Fatalf("competing claim = %v, %q, %v", ok, holder, err)
	}
	if err := store.ReleaseScoped(key, "run-a", claimrelease.Completed.Reason()); err != nil {
		t.Fatal(err)
	}
	if ok, _, err = store.ClaimScoped(key, "run-b", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim after release = %v, %v", ok, err)
	}
}

func testSchedulerState(t *testing.T, store localscheduler.SchedulerState, dir string) {
	t.Helper()
	id := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "scheduled"}
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if err := store.WriteScheduleDemand(dir, map[localscheduler.WorkflowIdentity]bool{id: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReadScheduleDemand(dir); err != nil || !got[id] {
		t.Fatalf("schedule demand = %+v, %v", got, err)
	}
	if err := store.WriteTriggerEvaluations(dir, map[localscheduler.WorkflowIdentity]time.Time{id: at}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReadTriggerEvaluations(dir); err != nil || !got[id].Equal(at) {
		t.Fatalf("trigger evaluations = %+v, %v", got, err)
	}
}

func testAcceptanceStore(t *testing.T, store triggerqueue.OrdinaryStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	record, repeated, err := store.Accept(ctx, "manual/1", "operator", []byte(`{"request":1}`), now)
	if err != nil || repeated {
		t.Fatalf("accept = %+v, %v, %v", record, repeated, err)
	}
	replay, repeated, err := store.Accept(ctx, "manual/1", "operator", []byte(`{"request":1}`), now.Add(time.Minute))
	if err != nil || !repeated || replay.ID != record.ID {
		t.Fatalf("accept replay = %+v, %v, %v", replay, repeated, err)
	}
	if err := store.BeginDispatch(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDispatch(ctx, record.ID, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, record.ID, triggerqueue.Dispatched, "run-1", "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func testScheduleCursor(t *testing.T, source triggerqueue.SourceAcceptanceStore, cursor triggerqueue.ScheduleCursorStore) {
	t.Helper()
	ctx := context.Background()
	before := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	after := before.Add(time.Minute)
	got, legacy, err := cursor.SourceCursorRevision(ctx, "g/workflow", "revision", before, before, false)
	if err != nil || legacy || !got.Equal(before) {
		t.Fatalf("initial cursor = %v, %v, %v", got, legacy, err)
	}
	_, repeated, err := source.AcceptSource(ctx, triggerqueue.SourceBatch{
		Key: "schedule/1", Actor: "scheduler", Fingerprint: "fingerprint",
		Advance: &triggerqueue.SourceAdvance{Scope: "g/workflow", Revision: "revision", Before: before, After: after},
	}, after)
	if err != nil || repeated {
		t.Fatalf("advance cursor = %v, %v", repeated, err)
	}
	got, _, err = cursor.SourceCursorRevision(ctx, "g/workflow", "revision", before, after, false)
	if err != nil || !got.Equal(after) {
		t.Fatalf("advanced cursor = %v, %v", got, err)
	}
}

func testPendingLimit(t *testing.T, store triggerqueue.SourceAcceptanceStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 5, 0, 0, time.UTC)
	limit := triggerqueue.WorkflowPendingLimit{Gaggle: "g", Workflow: "worker", MaxPending: 1}
	batch := triggerqueue.SourceBatch{
		Key: "workers/1", Actor: "scheduler", Fingerprint: "workers/1",
		Starts:       []triggerqueue.SourceStart{{Payload: []byte(`{"target":{"gaggle":"g","workflow":"worker"}}`)}},
		PendingLimit: &limit,
	}
	if _, _, err := store.AcceptSource(ctx, batch, now); err != nil {
		t.Fatal(err)
	}
	if count, err := store.PendingWorkflowStarts(ctx, limit); err != nil || count != 1 {
		t.Fatalf("pending starts = %d, %v", count, err)
	}
	batch.Key, batch.Fingerprint = "workers/2", "workers/2"
	if _, _, err := store.AcceptSource(ctx, batch, now.Add(time.Second)); !errors.Is(err, triggerqueue.ErrWorkerOccupancy) {
		t.Fatalf("over-cap acceptance = %v", err)
	}
}

func testInstanceStore(t *testing.T, store journal.InstanceStore) {
	t.Helper()
	if err := store.Append(journal.Event{Type: journal.EventDaemonStarted}); err != nil {
		t.Fatal(err)
	}
	seq, err := store.LastSeq()
	if err != nil || seq != 1 {
		t.Fatalf("last seq = %d, %v", seq, err)
	}
	if err := store.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadAfterSeq(seq)
	if err != nil || len(events) != 1 || events[0].Seq != 2 {
		t.Fatalf("events after %d = %+v, %v", seq, events, err)
	}
}

func testRunDirReconciler(t *testing.T, reconciler localscheduler.RunDirReconciler, runsDir string) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-active", Gaggle: "g", Workflow: "implementation",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	inventory, err := reconciler.FromRoots(context.Background(), []string{runsDir})
	id := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "implementation"}
	if err != nil || inventory.Active[id] != 1 || inventory.Runs["run-active"].Identity != id {
		t.Fatalf("run inventory = %+v, %v", inventory, err)
	}
}
