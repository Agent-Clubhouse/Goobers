package main

import (
	"io"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/platform/proc"
)

// reportStageMemoryBound states, at every daemon start, whether one stage's
// memory is bounded (#4070).
//
// WHY THIS IS A STARTUP LINE AND NOT A CHECK. Nothing here is misconfigured.
// An instance with no runner.stageMemoryLimit is valid, loads cleanly, and
// passes every gate — and it is also an instance where one stage can OOM-kill
// the control-plane daemon, because stage subprocesses share the daemon's own
// memory cgroup. Production hit that three times in one review window.
//
// The evidence then erases itself: memory.events counters live in the cgroup
// and reset with the container, so a post-hoc look reads `oom_kill 0` on a pod
// that was OOM-killed half an hour earlier, and only
// containerStatuses[].lastState.terminated still remembers. A hazard that
// leaves no trace and fails no check is one an operator can only learn about
// from the daemon volunteering it.
//
// The enforced case is reported too, not just the unenforced one: a bound that
// is configured but silently inert — no delegated cgroup, no fallback allowed
// — reads to an operator exactly like a bound that is working. Saying which
// mechanism is in force is the only report that distinguishes them.
func reportStageMemoryBound(cfg *instance.Config, stdout, stderr io.Writer) {
	if cfg == nil {
		return
	}
	bound, err := cfg.Runner.ResolveStageMemoryBound()
	if err != nil {
		// Already refused at config load; under --skip-preflight it can reach
		// here, and an unparseable limit enforces nothing.
		pf(stderr, "warning: startup: runner.stageMemoryLimit is unusable, so stages run unbounded: %v\n", err)
		return
	}
	if !bound.Enforced() {
		pf(stderr, "warning: startup: %s (%s)\n", instance.UnboundedStageMemoryWarning, bound.Source)
		return
	}
	// Probe rather than assert: this creates and releases a real child cgroup,
	// so a success is evidence the mechanism works — not a reading of intent.
	mechanism, detail := proc.ProbeMemoryBound(bound.MaxBytes, bound.AllowAddressSpaceFallback)
	switch mechanism {
	case proc.MechanismNone:
		pf(stderr, "warning: startup: runner.stageMemoryLimit is set (%s) but CANNOT BE ENFORCED here: %s. "+
			"Stages run unbounded inside this daemon's memory cgroup (#4070)\n", bound.Source, detail)
	case proc.MechanismRlimitAS:
		pf(stderr, "warning: startup: per-stage memory bound %d bytes (%s) enforced via RLIMIT_AS: %s. "+
			"RLIMIT_AS bounds ADDRESS SPACE, not resident memory, so a runtime that reserves more than it "+
			"touches can trip it — verify the value against a real stage\n", bound.MaxBytes, bound.Source, detail)
	default:
		pf(stdout, "startup: per-stage memory bound %d bytes (%s) enforced via %s\n",
			bound.MaxBytes, bound.Source, mechanism)
	}
}
