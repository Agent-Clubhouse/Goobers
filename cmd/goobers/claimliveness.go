package main

import (
	"context"
	"fmt"
	"io"
	"time"

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
	return engine.NewWorkflowLiveness(c, engineConfig.Namespace), c.Close, nil
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
// caller's post-rebuild recovery pass instead of running before the renewal
// set exists. `goobers up` always passes it; the one-shot `run`/`signal`
// paths pass it exactly when `engine:` is configured (oneShotClaimRecovery),
// and a pure mode-1 one-shot keeps its recover-at-setup behavior via the nil
// gate default.
func withClaimRecoveryGate(gate *localscheduler.RecoveryGate) schedulerSetupOption {
	return func(options *schedulerSetupOptions) {
		options.claimRecoveryGate = gate
	}
}

// rebuildClaimRenewalSet runs one DS6 renewal rebuild pass — the LEDGER's
// claims renewed for every holder the probe reports (or fails) live — and
// opens gate exactly when the pass's ledger write durably completed: from
// then on RecoverExpired-style reaping is permitted. probeErr carries a
// fail-live probe degradation (those holders WERE renewed); a non-nil
// renewErr means the pass did not complete and the gate stays closed. Shared
// by `goobers up` (startup rebuild + periodic-tick self-heal) and the
// one-shot paths (oneShotClaimRecovery.finish) so the machinery cannot
// drift apart.
func rebuildClaimRenewalSet(ctx context.Context, l instance.Layout, probe localscheduler.RunLivenessProbe, gate *localscheduler.RecoveryGate) (probeErr, renewErr error) {
	_, probeErr, renewErr = renewLiveClaims(ctx, l, probe, DefaultClaimLease)
	if renewErr != nil {
		return probeErr, renewErr
	}
	gate.MarkRenewalRebuilt()
	return probeErr, nil
}

// oneShotClaimRecovery is DS6 for the standalone `goobers run`/`goobers
// signal` paths (#3512 review, finding 2). Those paths hold the instance
// lock, which means the daemon is DOWN — precisely when claim renewals have
// stopped — so on an engine-configured instance the setup-time reap at
// buildSchedulerSetup's nil-gate default would fire in exactly the window
// where a live distributed run's lease looks expired. A gate alone is not
// enough: ClaimLedger.Claim treats an expired lease as claimable, so the
// triggered run's own backlog-query could still claim over it. The one-shot
// sequence is therefore gate (defer the setup reap) + liveness probe + a
// RENEWAL pass (pushing live holders' ExpiresAt forward) + only then the
// gated reap — the same machinery the daemon startup uses, before any
// scheduling/claiming proceeds.
//
// A pure mode-1 instance (no `engine:`) gets a nil *oneShotClaimRecovery:
// no gate, no probe, and the recover-at-setup behavior byte-identical to
// today's.
type oneShotClaimRecovery struct {
	gate *localscheduler.RecoveryGate
}

// newOneShotClaimRecovery peeks the instance config to decide the shape. An
// unreadable config yields nil — buildSchedulerSetup will surface the real
// load error itself.
func newOneShotClaimRecovery(l instance.Layout) *oneShotClaimRecovery {
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil || !cfg.EngineProjectionEnabled() {
		return nil
	}
	return &oneShotClaimRecovery{gate: localscheduler.NewRecoveryGate()}
}

// setupOptions returns the scheduler-setup options deferring the setup-time
// reap; empty on a mode-1 (nil) recovery.
func (r *oneShotClaimRecovery) setupOptions() []schedulerSetupOption {
	if r == nil {
		return nil
	}
	return []schedulerSetupOption{withClaimRecoveryGate(r.gate)}
}

// finish rebuilds the renewal set from ledger + liveness and then runs the
// reap the gate deferred. It must run BEFORE the caller schedules or claims
// anything. A failure is returned as an error the caller must treat as fatal:
// proceeding without the renewal pass is exactly the reap-a-live-run (or
// claim-over-a-live-lease) window this type exists to close. A probe
// degradation alone is NOT fatal — those holders were renewed fail-live — and
// is reported as a warning on stderr.
func (r *oneShotClaimRecovery) finish(ctx context.Context, l instance.Layout, setup *schedulerSetup, stderr io.Writer) error {
	if r == nil {
		return nil
	}
	probe, closeProbe, err := buildClaimLivenessProbe(setup.Config, setup.RunnerRegistry.RunIDs)
	if err != nil {
		return fmt.Errorf("build claim liveness probe: %w", err)
	}
	defer closeProbe()
	probeErr, renewErr := rebuildClaimRenewalSet(ctx, l, probe, r.gate)
	if renewErr != nil {
		return fmt.Errorf("rebuild claim renewal set (required before a standalone trigger may recover claims): %w", renewErr)
	}
	if probeErr != nil {
		pf(stderr, "warning: claim liveness probe degraded (renewed fail-live): %v\n", probeErr)
	}
	if _, err := recoverClaims(l, setup.InstanceLog, time.Now(), nil, r.gate); err != nil && !isJournaledClaimsLockTimeout(err) {
		return fmt.Errorf("recover expired claims: %w", err)
	}
	return nil
}
