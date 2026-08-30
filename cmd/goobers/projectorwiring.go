package main

import (
	"context"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readmodel/projector"
	"github.com/goobers/goobers/internal/readmodel/repair"
)

var newRepairSweeper = repair.New
var newRetentionLoop = readmodel.NewRetentionLoop

// startProjector runs the read model's sole writer (#1923, §6.1).
//
// # What starting it changes
//
// Before: `ingestRunTelemetry` projected a run inline, from the writer, at
// completion. A run written while the daemon was down was never projected, and
// nothing noticed — the read model was silently missing it until the next
// rebuild.
//
// After: writers record a watermark in intake.db, and this discovers and applies
// them. A watermark written while the daemon was down is still there at start,
// which is what the restart pass is for.
//
// # Why the restart pass is bounded
//
// It reprojects exactly two categories: runs with a pending watermark, and runs
// the read model records as non-terminal. A terminal run cannot advance, so
// re-reading it is work with a known-empty result. On the live instance that is
// tens of rows rather than 40,665 directories.
//
// # Why failures here are warnings
//
// A projector that cannot start leaves the read model frozen, not wrong: the
// journal-derived paths still answer every request, and the cutover flag gates
// whether anything reads the store at all. Refusing to start the daemon over it
// would turn a degraded optimisation into an outage.
func startProjector(
	ctx context.Context,
	store *readmodel.Store,
	watermarks *intake.Store,
	l instance.Layout,
	cfg *instance.Config,
) (func(), func() readmodel.RetentionStats, func() projector.Stats) {
	runsDirs, err := l.RunDirs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: resolve runs directories for projector: %v\n", err)
		return func() {}, nil, nil
	}

	// The change feed the SSE stream will tail (#1929). Wired here so the
	// projector wakes subscribers as part of the same commit path that writes
	// the change row, rather than through a second mechanism with its own
	// latency and failure modes — which is precisely what the filesystem poller
	// was.
	feed := readmodel.NewFeed(store)
	p := projector.New(store, watermarks, projector.Options{RunsDirs: runsDirs, Feed: feed})
	stop := p.Start(ctx)

	// The repair sweep (#1924). It runs continuously at a fixed I/O budget,
	// cycling, with a durable cursor — never on a request path and never taking
	// a journal lock. It is what makes the read model COMPLETE rather than
	// merely current: the projector applies what writers reported, and repair
	// finds what nobody did, in both directions.
	// Projection retention (#1932). The default is 90 days — product policy
	// decision (issue #3056) to age out runs beyond full-fidelity listability
	// in unattended-operation scenarios, with operators opting out by setting
	// projectionFullFidelityDays to 0 or negative (unbounded). The default is
	// deliberate: a zero-day window would age out every run on the first pass
	// (the most destructive possible reading of an off value), so unbounded
	// requires an explicit configuration decision. Projection aging is
	// independent of journal retention: aging a run out of the projection
	// removes no evidence — the journal is still there, and a rebuild would
	// re-admit the run if the floor were lowered.
	window := readmodel.RetentionDays(cfg.ProjectionFullFidelityRetentionDays())
	retention := newRetentionLoop(store, p, window, readmodel.RetentionOptions{})

	sweepCtx, stopSweep := context.WithCancel(ctx)
	go retention.Run(sweepCtx)
	sweeper := newRepairSweeper(store, p, watermarks, repair.Options{RunsDirs: runsDirs})
	go sweeper.Run(sweepCtx)

	// The restart pass runs after Start, so its commits go through the same
	// serialized loop as live ones. Running it before would mean two writers
	// existed briefly, which is the one thing the commit loop exists to prevent.
	if result, err := p.Restart(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: projector restart pass: %v\n", err)
	} else if result.Drained > 0 || result.Reprojected > 0 || result.Missing > 0 {
		fmt.Fprintf(os.Stderr, "projector restart: drained %d, reprojected %d, missing %d\n",
			result.Drained, result.Reprojected, result.Missing)
	}
	return func() {
		stopSweep()
		stop()
	}, retention.Stats, p.Stats
}
