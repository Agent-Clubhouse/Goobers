package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

// fakeOpener refuses a second open in the same epoch exactly as the backend does
// (the stream's owner token is already taken), so a consumer that opens its own
// log would fail here.
type fakeOpener struct {
	mu    sync.Mutex
	opens int
	taken map[int64]bool
	epoch *int64
	fail  error
}

func (f *fakeOpener) OpenSchedulerLog(context.Context, func(error)) (*journalpg.SchedulerLog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if f.fail != nil {
		return nil, f.fail
	}
	if f.taken[*f.epoch] {
		return nil, journal.ErrRunOwned
	}
	f.taken[*f.epoch] = true
	return &journalpg.SchedulerLog{}, nil
}

func TestASecondOpenInTheSameEpochReturnsTheSameLog(t *testing.T) {
	epoch := int64(3)
	opener := &fakeOpener{taken: map[int64]bool{}, epoch: &epoch}
	logs := schedulerLogs{retire: func(*journalpg.SchedulerLog) error { return nil }}
	handle := &lease.Handle{InstanceID: "i", Epoch: 3}
	first, err := logs.For(t.Context(), opener, handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := logs.For(t.Context(), opener, handle, nil)
	if err != nil || second != first {
		t.Fatalf("second open = %p, %v; want the same log %p and no ErrRunOwned", second, err, first)
	}
	if opener.opens != 1 {
		t.Fatalf("the stream was opened %d times in one epoch, want once", opener.opens)
	}
	// A new epoch reopens (the previous owner token no longer binds).
	epoch = 4
	next, err := logs.For(t.Context(), opener, &lease.Handle{InstanceID: "i", Epoch: 4}, nil)
	if err != nil || next == first {
		t.Fatalf("new epoch: %p, %v; want a reopened log", next, err)
	}
}

func TestConcurrentConsumersOfOneEpochShareOneOpen(t *testing.T) {
	epoch := int64(7)
	opener := &fakeOpener{taken: map[int64]bool{}, epoch: &epoch}
	logs := schedulerLogs{retire: func(*journalpg.SchedulerLog) error { return nil }}
	handle := &lease.Handle{InstanceID: "i", Epoch: 7}
	results := make([]*sharedSchedulerLog, 16)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			log, err := logs.For(t.Context(), opener, handle, nil)
			if err != nil {
				t.Error(err)
			}
			results[i] = log
		})
	}
	wg.Wait()
	for _, log := range results {
		if log != results[0] {
			t.Fatal("concurrent consumers got different logs")
		}
	}
	if opener.opens != 1 {
		t.Fatalf("opens = %d, want 1", opener.opens)
	}
}

func TestOnlyTheOpenerRetiresTheLogAndExactlyOnce(t *testing.T) {
	epoch := int64(5)
	opener := &fakeOpener{taken: map[int64]bool{}, epoch: &epoch}
	var retired atomic.Int32
	done := make(chan struct{}, 4)
	logs := schedulerLogs{retire: func(*journalpg.SchedulerLog) error {
		retired.Add(1)
		done <- struct{}{}
		return nil
	}}
	handle := &lease.Handle{InstanceID: "i", Epoch: 5}
	ctx, cancel := context.WithCancel(t.Context())
	scheduler, err := logs.For(ctx, opener, handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := logs.For(ctx, opener, handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The scheduler admission stops first (as Runtime.Admission.Stop does).
	if err := scheduler.Close(); err != nil {
		t.Fatal(err)
	}
	_ = scheduler.Close()
	if retired.Load() != 0 {
		t.Fatal("a consumer's Close retired the stream; the other consumer's appends would fail")
	}
	if err := hooks.Close(); err != nil || retired.Load() != 0 {
		t.Fatalf("second consumer Close = %v, retired %d", err, retired.Load())
	}
	cancel()
	<-done
	if got := retired.Load(); got != 1 {
		t.Fatalf("retired %d times at epoch end, want exactly once", got)
	}
}

func TestAFailedOpenIsNotRemembered(t *testing.T) {
	epoch := int64(2)
	opener := &fakeOpener{taken: map[int64]bool{}, epoch: &epoch, fail: errors.New("database unavailable")}
	logs := schedulerLogs{retire: func(*journalpg.SchedulerLog) error { return nil }}
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	if _, err := logs.For(t.Context(), opener, handle, nil); err == nil {
		t.Fatal("a failed open reported success")
	}
	opener.fail = nil
	if log, err := logs.For(t.Context(), opener, handle, nil); err != nil || log == nil {
		t.Fatalf("retry after a failed open = %v, %v", log, err)
	}
	if _, err := logs.For(t.Context(), nil, handle, nil); err == nil {
		t.Fatal("a missing backend was accepted")
	}
}
