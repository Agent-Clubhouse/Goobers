package main

import (
	"context"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readmodel/projector"
)

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
func startProjector(ctx context.Context, store *readmodel.Store, watermarks *intake.Store, l instance.Layout) func() {
	runsDirs, err := l.RunDirs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: resolve runs directories for projector: %v\n", err)
		return func() {}
	}

	p := projector.New(store, watermarks, projector.Options{RunsDirs: runsDirs})
	stop := p.Start(ctx)

	// The restart pass runs after Start, so its commits go through the same
	// serialized loop as live ones. Running it before would mean two writers
	// existed briefly, which is the one thing the commit loop exists to prevent.
	if result, err := p.Restart(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: projector restart pass: %v\n", err)
	} else if result.Drained > 0 || result.Reprojected > 0 || result.Missing > 0 {
		fmt.Fprintf(os.Stderr, "projector restart: drained %d, reprojected %d, missing %d\n",
			result.Drained, result.Reprojected, result.Missing)
	}
	return stop
}
