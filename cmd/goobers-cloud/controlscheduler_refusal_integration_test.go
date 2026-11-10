//go:build cloudintegration

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/startinput"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Exercise the production schedulerAdmission -> schedstart -> startintent path
// as goobers_controller. Refusals must settle before dispatch becomes uncertain.
func TestPermanentResolveRefusalFinishesTheRowOnce(t *testing.T) {
	w := newWorld(t)
	c := w.controller("refusal-controller", func(m *lease.Manager) *lease.Handle { return pgtest.Acquire(t, m) }, testEntry(4), false)
	if err := c.admission.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Stop while the test context is live; t.Cleanup runs after it is canceled.
	defer c.stop()

	records := map[string]triggerqueue.Record{}
	for _, key := range []string{"permanent", "transient", "dispatching", "racing"} {
		c.accept(1, key)
		record, err := c.store.ByKey(t.Context(), key+"0")
		if err != nil {
			t.Fatal(err)
		}
		records[key] = record
	}
	corrupt, _, err := c.store.Accept(t.Context(), "corrupt", "alice", []byte(`not json`), w.clock())
	if err != nil {
		t.Fatal(err)
	}
	records["corrupt"] = corrupt
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.store.BindChildBlobs(blobs)
	child, _, err := c.store.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{
		Identity: triggerqueue.ChildIdentity{
			ChildParent:     triggerqueue.ChildParent{Gaggle: testGaggle, ParentRunID: "parent"},
			StageOccurrence: "stage", InvocationKey: "call",
		},
		Actor: "parent-stage", Payload: []byte(`{"kind":"goobers.child-start/v1"}`), MaxChildren: 2,
	}, w.clock())
	if err != nil {
		t.Fatal(err)
	}
	records["child"], err = c.store.ByRun(t.Context(), child.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.store.BeginDispatch(t.Context(), records["dispatching"].ID); err != nil {
		t.Fatal(err)
	}

	classifier := &startinput.Resolver{Generations: pinnedGenerations{}, Config: func() *instance.Config { return nil }, InstanceID: pgtest.InstanceID}
	transientErr := errors.New("generation store unreachable")
	permanentErr := fmt.Errorf("resolve pinned start: %w", startinput.ErrPinDrift)
	var mu sync.Mutex
	resolves := map[string]int{}
	var reports []error
	c.admission.c.Report = func(err error) { mu.Lock(); defer mu.Unlock(); reports = append(reports, err) }
	originalResolve := c.admission.deps.Resolve
	recovered := false
	c.admission.deps.Resolve = func(ctx context.Context, record triggerqueue.Record) (engine.RunInput, error) {
		mu.Lock()
		resolves[record.ID]++
		mu.Unlock()
		switch record.ID {
		case records["permanent"].ID:
			return engine.RunInput{}, permanentErr
		case records["transient"].ID:
			if recovered {
				return originalResolve(ctx, record)
			}
			return engine.RunInput{}, transientErr
		case records["racing"].ID:
			// The page was Accepted, but another dispatcher claims it before refusal.
			if err := c.store.BeginDispatch(ctx, record.ID); err != nil {
				return engine.RunInput{}, err
			}
			return engine.RunInput{}, permanentErr
		default:
			return classifier.Resolve(ctx, record)
		}
	}
	type custody struct {
		state, reason string
		finished      *time.Time
	}
	read := func(key string) custody {
		t.Helper()
		var got custody
		err := w.admin.QueryRow(t.Context(), `SELECT state, COALESCE(reason,''), finished_at FROM control.acceptance WHERE id=$1`, records[key].ID).Scan(&got.state, &got.reason, &got.finished)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	tick := func() {
		t.Helper()
		if err := c.admission.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	first := read("permanent")
	tick()
	second := read("permanent")
	if first.state != "rejected" || first.reason != permanentErr.Error() || first.finished == nil || second.finished == nil || !first.finished.Equal(*second.finished) {
		t.Fatalf("permanent custody = %+v then %+v; want one rejection with the resolver reason", first, second)
	}
	for _, key := range []string{"transient", "child", "corrupt", "dispatching", "racing"} {
		want := "accepted"
		if key == "dispatching" || key == "racing" {
			want = "dispatching"
		}
		if got := read(key); got.state != want || got.finished != nil {
			t.Errorf("%s custody = %+v, want unfinished %s", key, got, want)
		}
	}
	for key, want := range map[string]int{"permanent": 1, "transient": 2, "child": 2, "corrupt": 2, "dispatching": 0, "racing": 1} {
		if got := resolves[records[key].ID]; got != want {
			t.Errorf("%s resolves = %d, want %d", key, got, want)
		}
	}
	if w.startCount() != 0 {
		t.Fatalf("refused rows reached engine: %d", w.startCount())
	}
	counts := map[string]int{}
	for _, err := range reports {
		switch {
		case errors.Is(err, acceptance.ErrPermanentStart):
			counts["permanent"]++
			if errors.Is(err, triggerqueue.ErrTransition) {
				counts["racing"]++
			}
		case errors.Is(err, acceptance.ErrCorruptStart):
			counts["corrupt"]++
		case errors.Is(err, transientErr):
			counts["transient"]++
		default:
			t.Errorf("unexpected report: %v", err)
		}
	}
	if len(reports) != 6 || counts["permanent"] != 2 || counts["racing"] != 1 || counts["corrupt"] != 2 || counts["transient"] != 2 {
		t.Fatalf("reports = %v (%v), want permanent once, race once, transient/corrupt twice, no child", reports, counts)
	}
	if err := c.store.RejectAccepted(t.Context(), records["permanent"].ID, "again", w.clock()); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("second rejection = %v", err)
	}
	if err := c.store.RejectAccepted(t.Context(), records["dispatching"].ID, "late", w.clock()); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("dispatching rejection = %v", err)
	}
	if err := c.store.RejectAccepted(t.Context(), records["child"].ID, "ordinary refusal", w.clock()); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("child rejection = %v", err)
	}

	// Transient failure really is retryable through admission; success resolves
	// once more, not again asynchronously after BeginDispatch.
	recovered = true
	tick()
	w.settle(1, "transient resolver recovered")
	if resolves[records["transient"].ID] != 3 {
		t.Fatalf("successful retry resolved %d times, want 3 total", resolves[records["transient"].ID])
	}
}
