package main

import (
	"fmt"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

// dialEngineLiveness is a test seam mirroring engineprojection.go's
// dialEngineProjection: the DS6 liveness probe dials its own engine client so
// its lifetime matches the claim ticker's, not the projection reconciler's.
var dialEngineLiveness = bootstrap.DialTemporal

// engineLivenessProbe is a test seam over the engine half of the composite
// probe. Tests inject a fake so daemon-level DS6 ordering tests need no
// Temporal server; the default builds the real describe-backed probe.
var engineLivenessProbe = func(cfg *instance.Config) (localscheduler.RunLivenessProbe, func(), error) {
	engineConfig := cfg.EffectiveEngineConfig()
	c, err := dialEngineLiveness(engineConfig.HostPort, engineConfig.Namespace)
	if err != nil {
		return nil, nil, err
	}
	return engine.NewWorkflowLiveness(c), c.Close, nil
}

// buildClaimLivenessProbe assembles DS6's run-liveness signal
// (docs/design/distributed-state-and-coordination.md §6/§10) for claim-lease
// renewal: tracked in-process (always) plus, when `engine:` is configured, an
// open engine workflow — the half a daemon restart cannot lose. A pure mode-1
// instance with no engine config gets exactly the tracked probe, so its
// renewal set is byte-for-byte today's "runs this process is tracking". The
// returned close func releases the engine client, if one was dialed. A var so
// daemon-level DS6 ordering tests can substitute a fake probe wholesale.
var buildClaimLivenessProbe = func(cfg *instance.Config, tracked func() []string) (localscheduler.RunLivenessProbe, func(), error) {
	probes := []localscheduler.RunLivenessProbe{localscheduler.TrackedRunLiveness(tracked)}
	closeProbe := func() {}
	if cfg.EngineProjectionEnabled() {
		engineProbe, closeEngine, err := engineLivenessProbe(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("dial engine for claim liveness: %w", err)
		}
		probes = append(probes, engineProbe)
		closeProbe = closeEngine
	}
	return localscheduler.CompositeRunLiveness(probes...), closeProbe, nil
}

// withClaimRecoveryGate threads DS6's startup-ordering gate into
// buildSchedulerSetup, so the setup-time expired-claim reap defers to the
// daemon's post-rebuild recovery pass instead of running before the renewal
// set exists. Only `goobers up` passes it; setup callers without a renewal
// loop (`run`, `signal`) keep their recover-at-setup behavior via the nil
// gate default.
func withClaimRecoveryGate(gate *localscheduler.RecoveryGate) schedulerSetupOption {
	return func(options *schedulerSetupOptions) {
		options.claimRecoveryGate = gate
	}
}
