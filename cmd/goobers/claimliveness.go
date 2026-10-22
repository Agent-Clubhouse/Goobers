package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// buildClaimLivenessProbe assembles the local run-liveness signal used for
// claim-lease renewal.
var buildClaimLivenessProbe = func(tracked func() []string) (localscheduler.RunLivenessProbe, func(), error) {
	return localscheduler.TrackedRunLiveness(tracked), func() {}, nil
}

// startupLocalRunLiveness bridges the empty registry before crash resume. It is
// used for the initial renewal ONLY: after recovery, a nonterminal journal is
// not evidence of execution and only tracked runners may renew local claims.
// Unresolvable or unreadable runs therefore receive at most one lease of grace
// during this startup, rather than being kept alive by each periodic sweep.
type startupLocalRunLiveness struct{ layout instance.Layout }

func (p startupLocalRunLiveness) RunLive(ctx context.Context, runID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Reconciliation's synthetic leases belong to a prior invocation, not a
	// resumable runner. Do not preserve these temporary mutation locks.
	if strings.Contains(runID, "/") {
		return false, nil
	}
	dir, err := p.layout.FindRunDir(runID)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return false, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return false, err
	}
	if identity.RunID != runID {
		return false, fmt.Errorf("claim holder %q has journal identity %q", runID, identity.RunID)
	}
	// Distributed journals do not establish engine liveness. The ordinary
	// engine probe remains authoritative for them.
	if identity.EngineDriven() {
		return false, nil
	}
	phase, err := reader.Phase()
	return phase == journal.PhaseRunning, err
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

func rebuildStartupClaimRenewalSet(ctx context.Context, l instance.Layout, steady localscheduler.RunLivenessProbe, gate *localscheduler.RecoveryGate) (probeErr, renewErr error) {
	return rebuildClaimRenewalSet(ctx, l, localscheduler.CompositeRunLiveness(steady, startupLocalRunLiveness{layout: l}), gate)
}
