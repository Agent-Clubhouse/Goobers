//go:build cloudintegration

package control

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type countingStarter struct{ starts atomic.Int32 }

func (s *countingStarter) Start(context.Context, engine.RunInput) (engine.StartResult, error) {
	s.starts.Add(1)
	return engine.StartResult{}, nil
}

// TestPermanentResolveRefusalFinishesTheRowOnce proves the controller finishes a
// permanently refused start as rejected (so the generation is not re-acquired
// and re-pinned on every sweep), while a transient refusal stays accepted and
// is retried. It runs as goobers_controller.
func TestPermanentResolveRefusalFinishesTheRowOnce(t *testing.T) {
	owner := pgtest.Pool(t, "permstart")
	config := owner.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store, err := acceptance.New(pool, pgtest.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	store.BindFence(manager, handle)
	permanent, _, err := store.Accept(t.Context(), "permanent", "operator", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	transient, _, err := store.Accept(t.Context(), "transient", "operator", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	child, _, err := store.Accept(t.Context(), "child-like", "operator", []byte(`{"kind":"goobers.child-start/v1"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	corrupt, _, err := store.Accept(t.Context(), "corrupt", "operator", []byte(`not json`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var resolves atomic.Int32
	var reports []error
	var reportsMu sync.Mutex
	runtime := Runtime{Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
		resolves.Add(1)
		if r.ID == child.ID {
			return engine.RunInput{}, fmt.Errorf("%w: kind child", acceptance.ErrNotOrdinaryStart)
		}
		if r.ID == corrupt.ID {
			return engine.RunInput{}, acceptance.ErrCorruptStart
		}
		if r.ID == permanent.ID {
			return engine.RunInput{}, fmt.Errorf("%w: retained generation differs from the pinned digests", acceptance.ErrPermanentStart)
		}
		return engine.RunInput{}, errors.New("generation store unreachable")
	}, Report: func(err error) {
		reportsMu.Lock()
		defer reportsMu.Unlock()
		reports = append(reports, err)
	}}
	starter := &countingStarter{}
	for range 2 {
		if _, err := runtime.drain(t.Context(), store, starter, ""); err != nil {
			t.Fatal(err)
		}
	}

	state := func(id string) (string, string) {
		var s, reason string
		if err := owner.QueryRow(t.Context(), `SELECT state, COALESCE(reason,'') FROM control.acceptance WHERE id=$1`, id).Scan(&s, &reason); err != nil {
			t.Fatal(err)
		}
		return s, reason
	}
	if s, reason := state(permanent.ID); s != "rejected" || reason == "" {
		t.Fatalf("permanent row = %q (%q), want rejected with the reason", s, reason)
	}
	if s, _ := state(transient.ID); s != "accepted" {
		t.Fatalf("transient row = %q, want accepted and retried", s)
	}
	if s, _ := state(child.ID); s != "accepted" {
		t.Fatalf("child-like row = %q, want it left alone by the ordinary start loop", s)
	}
	// permanent: resolved once; transient and child-like: resolved on both sweeps.
	if s, _ := state(corrupt.ID); s != "accepted" {
		t.Fatalf("corrupt row = %q, want it left accepted (visible, never rejected)", s)
	}
	if resolves.Load() != 7 || starter.starts.Load() != 0 {
		t.Fatalf("resolves = %d starts = %d, want 7 resolves and no start", resolves.Load(), starter.starts.Load())
	}
	if len(reports) != 5 { // permanent once, transient x2, corrupt x2; the child-like row is not reported
		t.Fatalf("reports = %v, want every refusal surfaced", reports)
	}
	// A row that already began dispatching cannot be rejected out from under the starter.
	if err := store.RejectAccepted(t.Context(), transient.ID, "late", time.Now()); err != nil {
		t.Fatalf("rejecting an accepted row: %v", err)
	}
	if err := store.RejectAccepted(t.Context(), transient.ID, "again", time.Now()); err == nil {
		t.Fatal("a finished row was rejected twice")
	}
}
