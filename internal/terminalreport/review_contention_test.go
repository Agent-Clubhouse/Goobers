package terminalreport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestReview7671NewProjectionSurvivesSlowReplay(t *testing.T) {
	now := time.Now()
	q := Queue{Dir: t.TempDir(), Now: func() time.Time { return now }}
	old := Effect{Kind: WorkFailure, ItemID: "42", Count: 2, RunID: "old-run"}
	if err := q.Publish(context.Background(), old, func(context.Context, Effect) error { return errors.New("offline") }); err == nil {
		t.Fatal("expected deferred intent")
	}
	now = now.Add(2 * time.Minute)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- q.Reconcile(context.Background(), 1, func(context.Context, Effect) error { close(started); <-release; return nil })
	}()
	<-started
	newer := old
	newer.Reset = true
	newer.Count = 0
	newer.RunID = "new-run"
	err := q.Publish(context.Background(), newer, func(context.Context, Effect) error { t.Error("must not apply concurrently"); return nil })
	close(release)
	if replayErr := <-done; replayErr != nil {
		t.Fatal(replayErr)
	}
	t.Logf("new projection Publish error: %v", err)
	delivered := false
	restarted := Queue{Dir: q.Dir, Now: q.Now}
	if err := restarted.Reconcile(context.Background(), 8, func(_ context.Context, e Effect) error {
		if e.Reset && e.RunID == "new-run" {
			delivered = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !delivered {
		t.Fatal("new reset intent lost while older provider replay held lock; fresh daemon has nothing to replay")
	}
}

func TestQueueExpiredLeaseCannotAcknowledgeReplacementLease(t *testing.T) {
	now := time.Now()
	q := Queue{Dir: t.TempDir(), Now: func() time.Time { return now }}
	e := Effect{Kind: WorkFailure, ItemID: "42", RunID: "old"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Publish(ctx, e, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	first, err := q.claim(context.Background(), effectKey(e))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	replacement, err := q.claim(context.Background(), effectKey(e))
	if err != nil {
		t.Fatal(err)
	}
	if err := q.acknowledge(context.Background(), effectKey(e), first, nil); err != nil {
		t.Fatal(err)
	}
	if err := q.freeze(context.Background(), effectKey(e), replacement, "replacement body"); err != nil {
		t.Fatalf("stale acknowledgement deleted current lease: %v", err)
	}
	if err := q.acknowledge(context.Background(), effectKey(e), replacement, nil); err != nil {
		t.Fatal(err)
	}
}

func TestQueueNewIntentSupersedesPreparationAndInheritsQuota(t *testing.T) {
	now := time.Now()
	q := Queue{Dir: t.TempDir(), Now: func() time.Time { return now }}
	old := Effect{Kind: WorkFailure, ItemID: "42", RunID: "old"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Publish(ctx, old, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	claimed, err := q.claim(context.Background(), effectKey(old))
	if err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.RunID = "new"
	newer.Reset = true
	if err := q.Publish(context.Background(), newer, func(context.Context, Effect) error { t.Fatal("concurrent delivery"); return nil }); !errors.Is(err, errDeliveryPending) {
		t.Fatal(err)
	}
	if err := q.freeze(context.Background(), effectKey(old), claimed, "old body"); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale preparation allowed: %v", err)
	}
	quota := &providers.RateLimitError{Reset: now.Add(time.Hour)}
	if err := q.acknowledge(context.Background(), effectKey(old), claimed, quota); !errors.Is(err, quota) {
		t.Fatal(err)
	}
	if err := q.Reconcile(context.Background(), 1, func(context.Context, Effect) error { t.Fatal("ignored old response quota"); return nil }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	delivered := false
	if err := q.Reconcile(context.Background(), 1, func(_ context.Context, e Effect) error { delivered = e.Reset && e.RunID == "new"; return nil }); err != nil {
		t.Fatal(err)
	}
	if !delivered {
		t.Fatal("new intent lost")
	}
}
