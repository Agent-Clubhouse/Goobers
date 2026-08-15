package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
)

var (
	engineProjectionInterval = 30 * time.Second
	dialEngineProjection     = bootstrap.DialTemporal
)

func startEngineProjection(ctx context.Context, l instance.Layout, set *instance.ConfigSet, watermarks *intake.Store, instanceLog *journal.InstanceLog, tel *telemetry.Client) (func(), error) {
	hostPort := strings.TrimSpace(os.Getenv("GOOBERS_TEMPORAL_HOSTPORT"))
	if hostPort == "" {
		return func() {}, nil
	}
	namespace := strings.TrimSpace(os.Getenv("GOOBERS_TEMPORAL_NAMESPACE"))
	if namespace == "" {
		namespace = "default"
	}
	c, err := dialEngineProjection(hostPort, namespace)
	if err != nil {
		return nil, err
	}
	runsDirs := make(map[string]string)
	for _, gaggle := range configuredGaggleNames(set) {
		runsDirs[gaggle] = l.ForGaggle(gaggle).RunsDir()
	}
	var observe engine.ProjectionObserver
	if watermarks != nil {
		observe = watermarks.Observed
	}
	reconciler, err := engine.NewCompletedRunReconciler(c, namespace, runsDirs, observe)
	if err != nil {
		c.Close()
		return nil, err
	}
	// Spans for tier-3 runs (#2865). Synthesized from the projection each time a
	// completed run is written, backdated to the run's own timestamps. nil when
	// telemetry is disabled, which the synthesizer treats as a no-op.
	reconciler = reconciler.WithSpans(newEngineSpanSink(tel))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	reporter := newSweepErrorReporter(instanceLog, "engine_projection_failed")
	go func() {
		defer close(done)
		ticker := time.NewTicker(engineProjectionInterval)
		defer ticker.Stop()
		for {
			_, err := reconciler.Reconcile(runCtx)
			reporter.report(err)
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
		c.Close()
	}, nil
}
