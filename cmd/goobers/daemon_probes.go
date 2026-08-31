package main

import (
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/httpapi"
)

// daemonProbeState is the wiring #3806's /healthz and /readyz read, pulled
// out of runUpContextWithForce into a named, directly unit-testable type —
// not two inline closures — so a change that regresses either the readiness
// gate (e.g. hardcoding Ready) or the liveness staleness check (e.g.
// hardcoding healthy) fails a fast, in-process test instead of shipping
// silently: neither closure this replaces had any test driving it directly
// before this type existed (only httpapi.WrapWithProbes' own stub-fed tests
// did, and TestUpServesUnauthenticatedProbesOnRealDaemon's happy path, which
// a hardcoded constant satisfies just as well).
//
// liveness deliberately reads an in-memory heartbeat (lastTickAtNanos)
// rather than stat'ing the on-disk lock file: this cluster's documented
// failure mode is a stalled RWO volume attachment, and a liveness probe
// that itself blocks on that same stalled disk cannot recover anything by
// being killed and restarted onto the same stuck volume. daemonstate.Refresh
// still writes the lock file's mtime, at its original once-per-completed-Tick
// cadence, for the cross-process readers (e.g. `goobers status`) that must
// see it there; only this in-process probe's read path is memory-only.
type daemonProbeState struct {
	ready           *atomic.Bool
	configLoaded    *atomic.Bool
	stateOpen       *atomic.Bool
	resumeComplete  *atomic.Bool
	sweepsStarted   *atomic.Bool
	schedulerTicked *atomic.Bool
	lastTickAtNanos *atomic.Int64 // unix nanoseconds; 0 = no tick recorded yet
	livenessTimeout time.Duration
	now             func() time.Time
}

// liveness implements httpapi.LivenessCheck.
func (d *daemonProbeState) liveness() bool {
	if !d.schedulerTicked.Load() {
		// Grace before the scheduler's first tick: a long legitimate
		// crash-resume (resumeInterruptedRunsWithRunners, unbounded, scales
		// with interrupted-run count) must not read as a wedged main loop.
		// Liveness is deliberately decoupled from startup/resume completion
		// — only a heartbeat that WAS established and then went stale
		// reports unhealthy; readiness, not liveness, gates on startup
		// actually finishing.
		return true
	}
	nanos := d.lastTickAtNanos.Load()
	if nanos == 0 {
		// schedulerTicked flipped true and the heartbeat mark has not been
		// observed yet only under a data race between the two atomics —
		// treat as grace rather than a false failure.
		return true
	}
	return daemonstate.Evaluate(d.now(), time.Unix(0, nanos), d.livenessTimeout).Healthy
}

// readiness implements httpapi.ReadinessCheck.
func (d *daemonProbeState) readiness() httpapi.ReadinessStatus {
	return httpapi.ReadinessStatus{
		// The single Ready gate every authenticated caller already sees on
		// /api/v1/health.Ready — never recomputed from Checks below, so the
		// two surfaces cannot drift out of lockstep.
		Ready: d.ready.Load(),
		Checks: map[string]bool{
			// configLoaded and stateOpen both flip before the HTTP listener
			// itself ever opens (runUpContextWithForce sets them, then calls
			// apiServer.Start() only afterward) — so in practice neither can
			// ever be observed false over HTTP; they are included anyway as
			// literal readiness diagnostics per #3806's own ask, informational
			// rather than load-bearing for this pair.
			"configLoaded":   d.configLoaded.Load(),
			"stateOpen":      d.stateOpen.Load(),
			"resumeComplete": d.resumeComplete.Load(),
			"sweepsStarted":  d.sweepsStarted.Load(),
		},
	}
}
