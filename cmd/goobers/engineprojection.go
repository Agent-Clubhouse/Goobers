package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
)

// engineProjectionClient is the Temporal surface this loop needs: the
// reconciler's own client slice. Narrowed from client.Client (which satisfies
// it) so the DAEMON WIRING BELOW is drivable by a fake — the reconciler's
// options, span source included, are configured here, and #3805 was a defect
// in exactly this kind of call site.
//
// Close is deliberately NOT part of it: since decision 003 step 1(e) this loop
// BORROWS `goobers up`'s one shared connection (enginerunguards.go) and closing
// it would take the DS6 liveness probe and the run guards down with it.
type engineProjectionClient interface {
	engine.CompletedRunClient
}

var (
	engineProjectionInterval = 30 * time.Second
	// projectionTemporalClient narrows the daemon's shared client to what the
	// reconciler needs. A var, and the replacement for the loop's own
	// dialEngineProjection, so the wiring below stays drivable by a fake
	// without the loop owning a second connection to the frontend.
	projectionTemporalClient = func(e *daemonEngineClient) engineProjectionClient {
		if c := e.Temporal(); c != nil {
			return c
		}
		return nil
	}
	// startEngineProjection is a var so up.go's call — and the blob store it
	// hands over — is assertable at the call site (#3805).
	startEngineProjection = launchEngineProjection
)

// launchEngineProjection runs the completed-run projection reconciler over the
// daemon's shared Temporal client. It does not dial and does not close:
// engineClient is owned by `goobers up`, which hands the same connection to
// the projection reconciler, the DS6 liveness probe and the engine-driven run
// guards. A client-less engineClient is the no-engine topology.
func launchEngineProjection(ctx context.Context, l instance.Layout, cfg *instance.Config, set *instance.ConfigSet, engineClient *daemonEngineClient, watermarks *intake.Store, instanceLog *journal.InstanceLog, tel *telemetry.Client, liveJournals *livejournal.Writer, blobs blobstore.Store) (func(), error) {
	if !cfg.EngineProjectionEnabled() {
		return func() {}, nil
	}
	c := projectionTemporalClient(engineClient)
	if c == nil {
		return func() {}, nil
	}
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
	}, nil
}
