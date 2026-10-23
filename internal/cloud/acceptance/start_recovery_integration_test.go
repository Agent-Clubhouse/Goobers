//go:build cloudintegration

package acceptance

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type recoveryStarter func(context.Context, engine.RunInput) (engine.StartResult, error)

func (f recoveryStarter) Start(ctx context.Context, in engine.RunInput) (engine.StartResult, error) {
	return f(ctx, in)
}

func TestPGRecoveryQuarantinesOnlyUncertainStartAndBacksOff(t *testing.T) {
	store, pool := testStore(t, 1)
	uncertain, _, err := store.Accept(t.Context(), "uncertain", "operator", []byte("pinned"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	healthy, _, err := store.Accept(t.Context(), "healthy", "operator", []byte("pinned"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{uncertain.ID, healthy.ID} {
		if err := store.BeginDispatch(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
		return engine.RunInput{AcceptanceID: r.ID, Gaggle: "web", WorkflowName: r.Key}, nil
	}
	temporal := &pgStartClient{}
	real := startfence.New(temporal, "cloud", NewStartAdmission(store))
	var uncertainCalls atomic.Int32
	starter := recoveryStarter(func(ctx context.Context, in engine.RunInput) (engine.StartResult, error) {
		if in.AcceptanceID == uncertain.ID {
			uncertainCalls.Add(1)
			return engine.StartResult{}, errors.New("Temporal ownership uncertain")
		}
		return real.Start(ctx, in)
	})
	if err := store.RecoverStarts(t.Context(), starter, resolve); err != nil {
		t.Fatal(err)
	}
	if uncertainCalls.Load() != 0 || temporal.executions != 0 {
		t.Fatal("recovery ignored grace period")
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.acceptance SET dispatch_started_at=clock_timestamp()-interval '2 minutes'"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverStarts(t.Context(), starter, resolve); err != nil {
		t.Fatal(err)
	}
	status, err := store.StartStatusPage(t.Context(), "", 100)
	if err != nil || len(status) != 1 || status[0].AcceptanceID != uncertain.ID || status[0].State != "start ownership uncertain" || status[0].RetryAt == nil {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	row, err := store.Get(t.Context(), healthy.ID, "operator")
	if err != nil || row.State != triggerqueue.Dispatched {
		t.Fatalf("healthy run blocked by uncertain peer: %+v %v", row, err)
	}
	if err := store.RecoverStarts(t.Context(), starter, resolve); err != nil {
		t.Fatal(err)
	}
	if uncertainCalls.Load() != 1 {
		t.Fatal("recovery ignored backoff")
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.acceptance SET start_retry_at=clock_timestamp()-interval '1 second' WHERE id=$1", uncertain.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverStarts(t.Context(), starter, resolve); err != nil {
		t.Fatal(err)
	}
	var retries int
	if err := pool.QueryRow(t.Context(), "SELECT start_retry_count FROM control.acceptance WHERE id=$1", uncertain.ID).Scan(&retries); err != nil {
		t.Fatal(err)
	}
	if retries != 2 || uncertainCalls.Load() != 2 {
		t.Fatalf("retries=%d attempts=%d", retries, uncertainCalls.Load())
	}
}

func TestPGRecoveryStaleLeaseCannotReserveRetries(t *testing.T) {
	old, pool := testStore(t, 1)
	in := acceptedStartInput(t, old)
	if err := old.BeginDispatch(t.Context(), in.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.acceptance SET dispatch_started_at=clock_timestamp()-interval '2 minutes'"); err != nil {
		t.Fatal(err)
	}
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	_ = successorStore(t, pool)
	if _, err := old.reserveStartRetries(t.Context()); err == nil {
		t.Fatal("stale recovery committed retry metadata")
	}
	var retries int
	if err := pool.QueryRow(t.Context(), "SELECT start_retry_count FROM control.acceptance WHERE id=$1", in.AcceptanceID).Scan(&retries); err != nil {
		t.Fatal(err)
	}
	if retries != 0 {
		t.Fatal("stale recovery changed retry metadata")
	}
}

func TestPGRecoverySkipsLockedRowsAndBoundsReservations(t *testing.T) {
	store, pool := testStore(t, 1)
	_, err := pool.Exec(t.Context(), `INSERT INTO control.acceptance
 (instance_id,id,idem_key,actor,fingerprint,payload,state,owner_epoch,run_id,accepted_at,dispatch_started_at)
 SELECT $1,'trigger-retry-'||n,'retry-'||n,'operator',decode('00','hex'),decode('78','hex'),
 'dispatching',$2,'retry-'||n,clock_timestamp()-interval '2 minutes',clock_timestamp()-interval '2 minutes'
 FROM generate_series(1,34) n`, store.instanceID, store.handle.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), "SELECT id FROM control.acceptance WHERE id='trigger-retry-1' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	rows, err := store.reserveStartRetries(t.Context())
	if err != nil || len(rows) != startRecoveryBatch {
		t.Fatalf("batch=%d err=%v", len(rows), err)
	}
	for _, row := range rows {
		if row.ID == "trigger-retry-1" {
			t.Fatal("recovery selected locked row")
		}
	}
	remaining, err := store.reserveStartRetries(t.Context())
	if err != nil || len(remaining) != 1 {
		t.Fatalf("remaining=%d err=%v", len(remaining), err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	last, err := store.reserveStartRetries(t.Context())
	if err != nil || len(last) != 1 || last[0].ID != "trigger-retry-1" {
		t.Fatalf("released lock=%+v err=%v", last, err)
	}
}

func TestPGFreshPredecessorRetriesOncePerLeaseThenBacksOff(t *testing.T) {
	first, pool := testStore(t, 1)
	record, _, err := first.Accept(t.Context(), "fresh-predecessor", "operator", []byte("pinned"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	if err := first.manager.Release(t.Context(), first.handle); err != nil {
		t.Fatal(err)
	}
	second := successorStore(t, pool)
	rows, err := second.reserveStartRetries(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("fresh predecessor was not retried: %v %v", rows, err)
	}
	rows, err = second.reserveStartRetries(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("same epoch bypassed backoff: %v %v", rows, err)
	}
	status, err := second.StartStatusPage(t.Context(), "", 100)
	if err != nil || len(status) != 1 {
		t.Fatalf("fresh quarantined start missing from status: %v %v", status, err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.acceptance SET start_retry_at=clock_timestamp()-interval '1 second' WHERE id=$1", record.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = second.reserveStartRetries(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("elapsed backoff still waited for initial grace: %v %v", rows, err)
	}
	if err := second.manager.Release(t.Context(), second.handle); err != nil {
		t.Fatal(err)
	}
	third := successorStore(t, pool)
	rows, err = third.reserveStartRetries(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("new epoch did not retry immediately: %v %v", rows, err)
	}
	row, err := third.Get(t.Context(), record.ID, "operator")
	if err != nil || row.State != triggerqueue.Dispatching {
		t.Fatalf("recovery read changed custody: %+v %v", row, err)
	}
}
