package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type stalledSchedulerMetricSink struct {
	shutdownDropExporter
	once    sync.Once
	started chan context.Context
	release chan struct{}
}

func (s *stalledSchedulerMetricSink) Export(ctx context.Context, _ *metricdata.ResourceMetrics) error {
	s.once.Do(func() { s.started <- ctx })
	<-s.release // Deliberately model a sink that ignores context cancellation.
	return nil
}

func (*stalledSchedulerMetricSink) Temporality(kind metric.InstrumentKind) metricdata.Temporality {
	return metric.DefaultTemporalitySelector(kind)
}

func TestBlockedSchedulerTelemetryDoesNotStallTickOrHealth(t *testing.T) {
	sink := &stalledSchedulerMetricSink{started: make(chan context.Context, 1), release: make(chan struct{})}
	tel, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName: "scheduler-test", Exporter: telemetry.ExporterStdout, Stdout: io.Discard,
		MetricExporter: sink, MetricExportInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	// No journal is installed: ingest is a no-op after the real sink is freed.
	setup := &schedulerSetup{Telemetry: tel, RollupDB: &rollup.DB{}, ProviderQuota: localscheduler.NewProviderQuotaState()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		close(sink.release)
		drain, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if setup.telemetryWork != nil {
			_ = setup.telemetryWork.Stop(drain)
		}
		_ = tel.Shutdown(drain)
	}()
	var now, heartbeat atomic.Int64
	var ticked atomic.Bool
	now.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }
	wake := make(chan time.Time)
	progress := make(chan struct{}, 10)
	starter := &capacityHoldingStarter{started: make(chan string, 1), release: make(chan struct{})}
	opts := setup.SchedulerOptions()
	opts = append(opts,
		localscheduler.WithClock(clock, func(time.Duration) <-chan time.Time { return wake }),
		localscheduler.WithTickHeartbeat(time.Minute, func(at time.Time) error {
			heartbeat.Store(at.UnixNano())
			ticked.Store(true)
			progress <- struct{}{}
			return nil
		}),
	)
	sched := localscheduler.New([]localscheduler.WorkflowEntry{{Workflow: "implementation", Starter: starter}}, nil, opts...)
	runID, err := sched.Trigger(ctx, "implementation", clock())
	if err != nil {
		t.Fatal(err)
	}
	if started := <-starter.started; started != runID {
		t.Fatalf("started %s, expected %s", started, runID)
	}
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()
	select {
	case sinkCtx := <-sink.started:
		if _, bounded := sinkCtx.Deadline(); !bounded {
			t.Fatal("scheduler telemetry has no deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("telemetry flush did not reach sink")
	}
	probes := &daemonProbeState{
		schedulerTicked: &ticked, lastTickAtNanos: &heartbeat,
		livenessTimeout: 2 * time.Minute, now: clock,
	}
	handler := httpapi.WrapWithProbes(http.NotFoundHandler(), probes.liveness, nil)
	for i := 0; i < 3; i++ {
		select {
		case <-progress:
		case <-time.After(2 * time.Second):
			t.Fatal("blocked telemetry prevented completed-tick heartbeat")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("healthz while sink stalled: %d %s", response.Code, response.Body.String())
		}
		if i < 2 {
			now.Add(int64(3 * time.Minute))
			wake <- clock()
		}
	}
	// The original run stays active throughout the delayed auxiliary work and
	// finishes normally when released; no restart or claim recovery is needed.
	select {
	case extra := <-starter.started:
		t.Fatalf("duplicate run admitted: %s", extra)
	default:
	}
	close(starter.release)
	sched.Wait()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("scheduler exit: %v", err)
	}
}

func TestSchedulerTelemetryDeadlineCoalescingAndShutdown(t *testing.T) {
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	work := newSchedulerTelemetryWork(10*time.Millisecond, func(ctx context.Context) {
		calls.Add(1)
		started <- ctx
		<-release
	})
	work.Notify(context.Background())
	ctx := <-started
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("telemetry deadline did not expire")
	}
	for i := 0; i < 100; i++ {
		work.Notify(context.Background())
	}
	if calls.Load() != 1 {
		t.Fatal("timed-out sink spawned overlapping telemetry passes")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := work.Stop(shutdown); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stuck telemetry shutdown: %v", err)
	}
	close(release)
	drain, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := work.Wait(drain); err != nil {
		t.Fatal(err)
	}
	work.Notify(context.Background())
	if calls.Load() != 1 {
		t.Fatal("telemetry restarted after shutdown")
	}
}
