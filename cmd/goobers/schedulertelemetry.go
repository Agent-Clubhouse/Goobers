package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const schedulerTelemetryTimeout = 15 * time.Second

// schedulerTelemetryWork coalesces tick notifications into at most one active
// telemetry pass. Notify is called under tickMu: it never waits for a sink,
// journal, or database. A subsequent tick (or shutdown's final flush) picks up
// journal records appended while a pass was busy.
//
// Even an exporter that ignores its deadline can occupy only one goroutine;
// timing out never permits overlapping passes against that exporter.
type schedulerTelemetryWork struct {
	mu      sync.Mutex
	timeout time.Duration
	run     func(context.Context)
	stopped bool
	done    chan struct{}
	cancel  context.CancelFunc
}

func newSchedulerTelemetryWork(timeout time.Duration, run func(context.Context)) *schedulerTelemetryWork {
	return &schedulerTelemetryWork{timeout: timeout, run: run}
}

func (w *schedulerTelemetryWork) Notify(parent context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.done != nil || parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, w.timeout)
	done := make(chan struct{})
	w.done, w.cancel = done, cancel
	go func() {
		defer cancel()
		w.run(ctx)
		w.mu.Lock()
		w.done, w.cancel = nil, nil
		close(done)
		w.mu.Unlock()
	}()
}

func (w *schedulerTelemetryWork) Wait(ctx context.Context) error {
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for scheduler telemetry flush/ingest: %w", ctx.Err())
	}
}

func (w *schedulerTelemetryWork) Stop(ctx context.Context) error {
	w.mu.Lock()
	w.stopped = true
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
	return w.Wait(ctx)
}
