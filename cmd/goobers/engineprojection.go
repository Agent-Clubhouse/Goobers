package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
)

var engineProjectionInterval = 30 * time.Second

// startEngineProjection runs the completed-run projection reconciler over the
// daemon's shared Temporal client (enginerunguards.go). It does not dial and
// does not close: engineClient is owned by `goobers up`, which hands the same
// connection to the projection reconciler, the DS6 liveness probe and the
// engine-driven run guards. A nil engineClient is the no-engine topology.
func startEngineProjection(ctx context.Context, l instance.Layout, cfg *instance.Config, set *instance.ConfigSet, engineClient *daemonEngineClient, watermarks *intake.Store, instanceLog *journal.InstanceLog, tel *telemetry.Client, liveJournals *livejournal.Writer) (func(), error) {
	if !cfg.EngineProjectionEnabled() || engineClient == nil {
		return func() {}, nil
	}
	c := engineClient.Temporal()
	engineConfig := cfg.EffectiveEngineConfig()
	runsDirs := make(map[string]string)
	for _, gaggle := range configuredGaggleNames(set) {
		runsDirs[gaggle] = l.ForGaggle(gaggle).RunsDir()
	}
	var observe engine.ProjectionObserver
	if watermarks != nil {
		observe = watermarks.Observed
	}
	reconciler, err := engine.NewCompletedRunReconciler(c, engineConfig.Namespace, runsDirs, observe)
	if err != nil {
		return nil, err
	}
	// Spans for tier-3 runs (#2865). Synthesized from the projection each time a
	// completed run is written, backdated to the run's own timestamps. nil when
	// telemetry is disabled, which the synthesizer treats as a no-op.
	reconciler = reconciler.WithSpans(newEngineSpanSink(tel))
	if liveJournals != nil {
		// DS5: the reconciler is repair/verify, never a second writer, for
		// runs the live writer authored — skip open journals, verify complete
		// ones, and file divergences to the named parity channel.
		reconciler = reconciler.
			WithLiveJournals(liveJournals).
			WithDivergenceReporter(liveJournalDivergenceReporter(instanceLog))
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	reporter := newSweepErrorReporter(instanceLog, "engine_projection_failed")
	go func() {
		defer close(done)
		ticker := time.NewTicker(engineProjectionInterval)
		defer ticker.Stop()
		for {
			if liveJournals != nil {
				liveJournals.CloseIdle(liveJournalIdleClose)
			}
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
	}, nil
}
