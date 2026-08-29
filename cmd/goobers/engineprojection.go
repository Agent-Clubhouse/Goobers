package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
)

var (
	engineProjectionInterval = 30 * time.Second
	dialEngineProjection     = bootstrap.DialTemporal
)

func startEngineProjection(ctx context.Context, l instance.Layout, cfg *instance.Config, set *instance.ConfigSet, watermarks *intake.Store, instanceLog *journal.InstanceLog, tel *telemetry.Client, liveJournals *livejournal.Writer, blobs blobstore.Store) (func(), error) {
	if !cfg.EngineProjectionEnabled() {
		return func() {}, nil
	}
	engineConfig := cfg.EffectiveEngineConfig()
	c, err := dialEngineProjection(engineConfig.HostPort, engineConfig.Namespace)
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
	reconciler, err := engine.NewCompletedRunReconciler(c, engineConfig.Namespace, runsDirs, observe)
	if err != nil {
		c.Close()
		return nil, err
	}
	// Spans for tier-3 runs (#2865). Synthesized from the projection each time a
	// completed run is written, backdated to the run's own timestamps. nil when
	// telemetry is disabled, which the synthesizer treats as a no-op.
	reconciler = reconciler.WithSpans(newEngineSpanSink(tel))
	// The SAME blob store newLiveJournalWriter adopts spans from (#3805).
	// Both of the reconciler's projections need it: the repair/backfill write
	// so a recovered run keeps its transcripts, and — the load-bearing half —
	// the DS5 verification re-projection, so a span the live writer adopted
	// does not re-project as a conformance-normative span_unavailable error
	// event and get filed as a divergence on every agentic run.
	reconciler = reconciler.WithSpanSource(blobs)
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
		c.Close()
	}, nil
}
