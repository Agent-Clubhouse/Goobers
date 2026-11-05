package sharedclaims

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
)

type loopAwaiters struct {
	attachErr error
	ticks     atomic.Int32
	stops     atomic.Int32
}

func (a *loopAwaiters) Reattach(context.Context, *lease.Handle, map[string]bool) error {
	return a.attachErr
}
func (a *loopAwaiters) Tick(context.Context) error { a.ticks.Add(1); return nil }
func (a *loopAwaiters) Stop(context.Context) error { a.stops.Add(1); return nil }

func TestLabelLoopDoesNotBlockAwaitersAndJoinsOnStop(t *testing.T) {
	base := &loopAwaiters{}
	started, stopped := make(chan struct{}), make(chan struct{})
	wrapped := WithReconciler(base, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}, func(err error) { t.Errorf("shutdown cancellation reported: %v", err) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := wrapped.Reattach(ctx, &lease.Handle{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("label sweep did not start")
	}
	if err := wrapped.Tick(ctx); err != nil || base.ticks.Load() != 1 {
		t.Fatalf("slow labels blocked terminal awaiters: %v", err)
	}
	if err := wrapped.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Stop returned before label repair finished")
	}
	if base.stops.Load() != 1 {
		t.Fatal("underlying awaiters were not stopped")
	}
}

func TestLabelFailureIsReportedWithoutFailingEpochAttachment(t *testing.T) {
	failure := errors.New("provider unavailable")
	reported := make(chan error, 1)
	wrapped := WithReconciler(&loopAwaiters{}, func(context.Context) error { return failure }, func(err error) { reported <- err })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := wrapped.Reattach(ctx, &lease.Handle{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reported:
		if !errors.Is(err, failure) {
			t.Fatalf("reported=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("provider failure was not reported")
	}
	if err := wrapped.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLabelLoopDoesNotStartAfterFailedAttachment(t *testing.T) {
	failure := errors.New("reattach failed")
	wrapped := WithReconciler(&loopAwaiters{attachErr: failure}, func(context.Context) error {
		t.Error("sweep started after failed attachment")
		return nil
	}, nil)
	if err := wrapped.Reattach(t.Context(), &lease.Handle{}, nil); !errors.Is(err, failure) {
		t.Fatalf("attachment=%v", err)
	}
	if err := wrapped.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}
