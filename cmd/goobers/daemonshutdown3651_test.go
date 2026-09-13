package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// #3651: a close that never returns used to hang `goobers up` forever because
// shutdown ran on an unbounded background context. Shutdown must give up and
// name the step it was waiting on.
func TestSchedulerSetupShutdownBoundsWedgedStep(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	restore := schedulerShutdownGrace
	schedulerShutdownGrace = 50 * time.Millisecond
	t.Cleanup(func() { schedulerShutdownGrace = restore })

	setup := &schedulerSetup{StopProjector: func() { <-release }}
	done := make(chan error, 1)
	go func() { done <- setup.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Shutdown returned nil for a wedged step")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error = %v, want context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "read model projector") {
			t.Fatalf("Shutdown error %q does not name the wedged step", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return: shutdown is unbounded")
	}
}

// #3651: shutdown must not report clean completion after losing final
// persisted state — every step's failure is joined into the returned error.
func TestRunShutdownStepsJoinsFailures(t *testing.T) {
	flushErr := errors.New("flush wedged")
	closeErr := errors.New("db locked")
	var order []string
	err := runShutdownSteps(context.Background(), []shutdownStep{
		{"telemetry client", func() error { order = append(order, "telemetry client"); return flushErr }},
		{"read model store", func() error { order = append(order, "read model store"); return nil }},
		{"instance log", func() error { order = append(order, "instance log"); return closeErr }},
	})
	if err == nil {
		t.Fatal("runShutdownSteps returned nil despite failing steps")
	}
	if !errors.Is(err, flushErr) || !errors.Is(err, closeErr) {
		t.Fatalf("runShutdownSteps error = %v, want both step errors joined", err)
	}
	for _, want := range []string{"shut down telemetry client", "shut down instance log"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("runShutdownSteps error %q missing %q", err, want)
		}
	}
	if strings.Join(order, ",") != "telemetry client,read model store,instance log" {
		t.Fatalf("steps ran out of order: %v", order)
	}
}

// A successful shutdown still reports success, and closing twice (explicit
// call plus the caller's safety-net defer) must not close anything twice.
func TestSchedulerSetupShutdownSucceedsOnceAndIsRepeatable(t *testing.T) {
	var stops atomic.Int64
	setup := &schedulerSetup{StopProjector: func() { stops.Add(1) }}
	if err := setup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := setup.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("StopProjector called %d times, want 1", got)
	}
}

// Nil-safety (#129): a caller defers Shutdown before knowing whether telemetry
// or the read model were ever constructed.
func TestSchedulerSetupShutdownNilSafe(t *testing.T) {
	var setup *schedulerSetup
	if err := setup.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil setup Shutdown: %v", err)
	}
	if err := (&schedulerSetup{}).Shutdown(context.Background()); err != nil {
		t.Fatalf("empty setup Shutdown: %v", err)
	}
}

// #4873: the final scheduler ingest is itself a best-effort writer. Keep the
// telemetry metric provider alive until both its ingest failure and the
// resulting diagnostic-append failure cross the observable discard boundary.
func TestSchedulerSetupShutdownExportsFinalIngestAppendDrop(t *testing.T) {
	exporter := &shutdownDropExporter{}
	tel, err := telemetry.New(context.Background(), telemetry.Config{
		Exporter:             telemetry.ExporterStdout,
		Stdout:               io.Discard,
		MetricExporter:       exporter,
		MetricExportInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(filepath.Join(dir, "scheduler"), journal.WithInstanceAppendDropObserver(tel))
	if err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(filepath.Join(dir, "rollup.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Force the final ingest to fail and its best-effort diagnostic append to
	// fail independently. Shutdown must still run every later close/flush.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	setup := &schedulerSetup{Telemetry: tel, RollupDB: db, InstanceLog: log}
	if err := setup.Shutdown(context.Background()); err == nil || !strings.Contains(err.Error(), "scheduler telemetry ingest") {
		t.Fatalf("Shutdown error = %v, want final scheduler ingest failure", err)
	}
	if got := log.Stats().AppendsDropped; got != 1 {
		t.Fatalf("final ingest dropped appends = %d, want 1", got)
	}
	if observed, afterShutdown := exporter.result(); !observed || afterShutdown {
		t.Fatalf("drop metric exported = %t, export after exporter shutdown = %t; want true, false", observed, afterShutdown)
	}
}

type shutdownDropExporter struct {
	mu                  sync.Mutex
	shutdown            bool
	observedDrop        bool
	exportedAfterClosed bool
}

func (*shutdownDropExporter) Temporality(kind metric.InstrumentKind) metricdata.Temporality {
	return metric.DefaultTemporalitySelector(kind)
}

func (*shutdownDropExporter) Aggregation(kind metric.InstrumentKind) metric.Aggregation {
	return metric.DefaultAggregationSelector(kind)
}

func (e *shutdownDropExporter) Export(_ context.Context, collected *metricdata.ResourceMetrics) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.shutdown {
		e.exportedAfterClosed = true
	}
	for _, scope := range collected.ScopeMetrics {
		for _, measurement := range scope.Metrics {
			if measurement.Name != telemetry.MetricJournalAppendsDropped {
				continue
			}
			if sum, ok := measurement.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) == 1 && sum.DataPoints[0].Value >= 1 {
				e.observedDrop = true
			}
		}
	}
	return nil
}

func (*shutdownDropExporter) ForceFlush(context.Context) error { return nil }

func (e *shutdownDropExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdown = true
	return nil
}

func (e *shutdownDropExporter) result() (bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.observedDrop, e.exportedAfterClosed
}
