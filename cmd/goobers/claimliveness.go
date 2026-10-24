package main

import (
	"context"

	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

var buildClaimLivenessProbe = scheduling.BuildClaimLivenessProbe

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
func rebuildClaimRenewalSet(ctx context.Context, l instance.Layout, probe localscheduler.RunLivenessProbe, gate *localscheduler.RecoveryGate) (error, error) {
	return claimLifecycle().RebuildRenewalSet(ctx, l, probe, gate, DefaultClaimLease)
}
func rebuildStartupClaimRenewalSet(ctx context.Context, l instance.Layout, probe localscheduler.RunLivenessProbe, gate *localscheduler.RecoveryGate) (error, error) {
	return claimLifecycle().RebuildStartupRenewalSet(ctx, l, probe, gate, DefaultClaimLease)
}
