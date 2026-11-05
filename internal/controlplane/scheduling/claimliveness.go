package scheduling

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// BuildClaimLivenessProbe assembles the local run-liveness signal used for
// claim-lease renewal.
var BuildClaimLivenessProbe = func(tracked func() []string) (localscheduler.RunLivenessProbe, func(), error) {
	return localscheduler.TrackedRunLiveness(tracked), func() {}, nil
}

// StartupLocalRunLiveness bridges the empty registry before crash resume. It is
// used for the initial renewal ONLY: after recovery, a nonterminal journal is
// not evidence of execution and only tracked runners may renew local claims.
// Unresolvable or unreadable runs therefore receive at most one lease of grace
// during this startup, rather than being kept alive by each periodic sweep.
type StartupLocalRunLiveness struct{ Layout instance.Layout }

// RunLive reports whether the startup journal proves a resumable local run.
func (p StartupLocalRunLiveness) RunLive(ctx context.Context, runID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Reconciliation's synthetic leases belong to a prior invocation, not a
	// resumable runner. Do not preserve these temporary mutation locks.
	if strings.Contains(runID, "/") {
		return false, nil
	}
	dir, err := p.Layout.FindRunDir(runID)
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

// RebuildRenewalSet renews the ledger claims for every holder the probe reports
// live or cannot conclusively classify. It opens gate only after the renewal
// write completes durably, permitting expired-claim recovery. probeErr reports
// degraded liveness probes whose holders were still renewed; renewErr means the
// renewal did not complete and the gate stays closed. Startup, periodic and
// one-shot recovery use this same renewal gate.
func (c ClaimLifecycle) RebuildRenewalSet(ctx context.Context, l instance.Layout, probe localscheduler.RunLivenessProbe, gate *localscheduler.RecoveryGate, leaseDuration time.Duration) (probeErr, renewErr error) {
	_, probeErr, renewErr = c.RenewLive(ctx, l, probe, leaseDuration)
	if renewErr != nil {
		return probeErr, renewErr
	}
	gate.MarkRenewalRebuilt()
	return probeErr, nil
}

// RebuildStartupRenewalSet adds one startup grace pass to steady-state liveness.
func (c ClaimLifecycle) RebuildStartupRenewalSet(ctx context.Context, l instance.Layout, steady localscheduler.RunLivenessProbe, gate *localscheduler.RecoveryGate, leaseDuration time.Duration) (probeErr, renewErr error) {
	return c.RebuildRenewalSet(ctx, l, localscheduler.CompositeRunLiveness(steady, StartupLocalRunLiveness{Layout: l}), gate, leaseDuration)
}
