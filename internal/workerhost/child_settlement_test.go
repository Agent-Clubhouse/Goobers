package workerhost

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunChildSettlementExpiryReportsAbandonedWork(t *testing.T) {
	h := newTestHost(t, Config{TaskQueues: []string{"engine"}}, &fakeFleet{})
	h.childSettlementTimeout = 25 * time.Millisecond
	h.tracker.n.Add(1)
	h.tracker.children.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := h.Run(ctx)
	if !errors.Is(err, ErrAbandonedWork) || time.Since(start) < h.childSettlementTimeout {
		t.Fatal("child cleanup was skipped or falsely completed", err, time.Since(start))
	}
}

func TestRunChildSettlementAlsoWaitsAfterQueueStartFailure(t *testing.T) {
	fleet := &fakeFleet{startErr: map[string]error{"second": errors.New("start failed")}}
	h := newTestHost(t, Config{TaskQueues: []string{"first", "second"}}, fleet)
	h.childSettlementTimeout = time.Second
	h.tracker.n.Add(1)
	h.tracker.children.Add(1)
	done := make(chan error, 1)
	go func() { done <- h.Run(context.Background()) }()
	select {
	case err := <-done:
		t.Fatal("returned before child settlement", err)
	case <-time.After(30 * time.Millisecond):
	}
	h.tracker.n.Add(-1)
	h.tracker.children.Add(-1)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("lost queue start error")
		}
	case <-time.After(time.Second):
		t.Fatal("host did not finish after child settlement")
	}
}
