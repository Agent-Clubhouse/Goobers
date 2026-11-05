package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/cloud/terminalcompose"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

type fakeEpochClaims struct{}

func (fakeEpochClaims) AdoptAll(context.Context, *lease.Handle) (int64, error) { return 0, nil }
func (fakeEpochClaims) RenewRuns(context.Context, *lease.Handle, map[string]bool) ([]localscheduler.ClaimEntry, error) {
	return nil, nil
}
func (fakeEpochClaims) RecoverExpired(context.Context, *lease.Handle, interface{ RecoveryPermitted() bool }, func(context.Context, string) (bool, error)) ([]localscheduler.ClaimEntry, error) {
	return nil, nil
}

type fakeEpochAwaiters struct{ stopped atomic.Int32 }

func (*fakeEpochAwaiters) Reattach(context.Context, *lease.Handle, map[string]bool) error {
	return nil
}
func (*fakeEpochAwaiters) Tick(context.Context) error { return nil }
func (a *fakeEpochAwaiters) Stop(context.Context) error {
	a.stopped.Add(1)
	return nil
}

type fakeTemporal struct{ epoch.Temporal }

func withEpochWiring(t *testing.T, parts epochWiringParts) {
	t.Helper()
	original := epochWiring
	t.Cleanup(func() { epochWiring = original; epochTemporal.Store(nil); terminalStatus.Store(nil) })
	epochWiring = parts
	var temporal epoch.Temporal = fakeTemporal{}
	epochTemporal.Store(&temporal)
}

func constructedEpoch(t *testing.T) func(context.Context, *lease.Handle) (control.EpochServices, error) {
	t.Helper()
	var planes controlPlanes
	// A lazily connecting pool: lease.New needs a real one, and nothing here
	// reaches the database.
	poolConfig, err := pgxpool.ParseConfig("postgres://goobers@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.MaxConns = lease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := databaseServices{Pools: &cloudpg.Pools{Direct: pool}}
	if err := constructEpoch(context.Background(), db, controlAuth{}, nil, nil, &planes); err != nil {
		t.Fatal(err)
	}
	return planes.Epoch
}

var testHandle = &lease.Handle{InstanceID: "i", Name: lease.ControllerName, Holder: "h", Epoch: 1}

// The Epoch seam refuses by name until the terminal hooks are composed: a
// controller must never serve engine runs without them.
func TestEpochRefusesWithoutTerminalHooks(t *testing.T) {
	withEpochWiring(t, epochWiringParts{
		Claims:       func(*lease.Manager, *lease.Handle) (epoch.Claims, error) { return fakeEpochClaims{}, nil },
		Observations: func() epoch.Observations { return epoch.Observations{} },
	})
	_, err := constructedEpoch(t)(context.Background(), testHandle)
	if err == nil || !strings.Contains(err.Error(), "terminal hooks") {
		t.Fatalf("Epoch without terminal hooks = %v, want a refusal naming them", err)
	}
}

// With the parts composed, the awaiters are the epoch's awaiters and the
// delivery loop runs as its L3.
func TestEpochRunsTheTerminalAwaitersAndDeliveryLoop(t *testing.T) {
	awaiters := &fakeEpochAwaiters{}
	var delivered atomic.Int32
	withEpochWiring(t, epochWiringParts{
		Claims: func(*lease.Manager, *lease.Handle) (epoch.Claims, error) { return fakeEpochClaims{}, nil },
		Observations: func() epoch.Observations {
			return epoch.Observations{Restore: func(context.Context, []control.Observation) error { return nil }, Capture: func(context.Context) ([]control.Observation, error) { return nil, nil }}
		},
		Terminal: func(context.Context, *lease.Manager, *lease.Handle) (terminalEpoch, error) {
			return terminalEpoch{Awaiters: awaiters, Delivery: func(context.Context) error { delivered.Add(1); return nil }}, nil
		},
	})
	services, err := constructedEpoch(t)(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if err := services.L3(context.Background()); err != nil || delivered.Load() != 1 {
		t.Fatalf("L3 = %v, deliveries %d; want the delivery loop", err, delivered.Load())
	}
	if err := services.Stop(context.Background()); err != nil || awaiters.stopped.Load() != 1 {
		t.Fatalf("Stop = %v, awaiters stopped %d; want the awaiters stopped with the epoch", err, awaiters.stopped.Load())
	}
}

// A terminal composition failure (the fenced log cannot be opened) fails the
// epoch build rather than serving without hooks.
func TestEpochBuildFailsWhenTheTerminalPartFails(t *testing.T) {
	withEpochWiring(t, epochWiringParts{
		Claims:       func(*lease.Manager, *lease.Handle) (epoch.Claims, error) { return fakeEpochClaims{}, nil },
		Observations: func() epoch.Observations { return epoch.Observations{} },
		Terminal: func(context.Context, *lease.Manager, *lease.Handle) (terminalEpoch, error) {
			return terminalEpoch{}, errors.New("scheduler log unavailable")
		},
	})
	if _, err := constructedEpoch(t)(context.Background(), testHandle); err == nil || !strings.Contains(err.Error(), "scheduler log unavailable") {
		t.Fatalf("Epoch = %v, want the terminal failure", err)
	}
}

// The outbox is visible: a readiness check and a lag figure while an epoch
// publishes its status, nothing on a standby, and a stopped epoch withdraws only
// its own publication.
func TestTerminalOutboxStatusIsPublishedAndWithdrawn(t *testing.T) {
	t.Cleanup(func() { terminalStatus.Store(nil) })
	terminalStatus.Store(nil)
	if terminalOutboxChecks() != nil || currentTerminalOutboxLag() != nil {
		t.Fatal("a standby reported outbox state")
	}
	status := func() terminalcompose.Status {
		return terminalcompose.Status{Pending: 3, OldestAge: 90 * time.Second, Healthy: false}
	}
	terminalStatus.Store(&status)
	if checks := terminalOutboxChecks(); checks == nil || checks["terminalOutbox"] || !checks["blockedCycleEscalation"] {
		t.Fatalf("checks = %v, want terminalOutbox false while unhealthy and no refused escalation", checks)
	}
	if lag := currentTerminalOutboxLag(); lag == nil || lag.Pending != 3 || lag.OldestSeconds != 90 || lag.Healthy {
		t.Fatalf("lag = %+v", lag)
	}
	other := func() terminalcompose.Status { return terminalcompose.Status{Healthy: true} }
	stale := &clearingAwaiters{Awaiters: &fakeEpochAwaiters{}, published: &other}
	if err := stale.Stop(context.Background()); err != nil || terminalOutboxChecks() == nil {
		t.Fatalf("an old epoch's stop withdrew the live epoch's status (err %v)", err)
	}
	live := &clearingAwaiters{Awaiters: &fakeEpochAwaiters{}, published: &status}
	if err := live.Stop(context.Background()); err != nil || terminalOutboxChecks() != nil {
		t.Fatalf("the live epoch's stop did not withdraw its status (err %v)", err)
	}
}

// With no planes to compose from, the default terminal part is absent and the
// seam says so, instead of composing a half-wired frame.
func TestTerminalPartIsAbsentWithoutItsInputs(t *testing.T) {
	if part := terminalPart(databaseServices{}, &controlPlanes{}); part != nil {
		t.Fatal("terminalPart composed without the claim plane, journal backend or Temporal")
	}
}

// A build failure after the terminal part published its status must not leave a
// stale outbox status behind for the readiness probe.
func TestAFailedEpochBuildWithdrawsThePublishedStatus(t *testing.T) {
	awaiters := &fakeEpochAwaiters{}
	status := func() terminalcompose.Status { return terminalcompose.Status{Healthy: true} }
	withEpochWiring(t, epochWiringParts{
		Claims: func(*lease.Manager, *lease.Handle) (epoch.Claims, error) { return fakeEpochClaims{}, nil },
		// Observations without restore/capture make epoch.Build refuse.
		Observations: func() epoch.Observations { return epoch.Observations{} },
		Terminal: func(context.Context, *lease.Manager, *lease.Handle) (terminalEpoch, error) {
			terminalStatus.Store(&status)
			return terminalEpoch{Awaiters: &clearingAwaiters{Awaiters: awaiters, published: &status}}, nil
		},
	})
	if _, err := constructedEpoch(t)(context.Background(), testHandle); err == nil {
		t.Fatal("epoch.Build accepted an incomplete configuration")
	}
	if terminalOutboxChecks() != nil || awaiters.stopped.Load() != 1 {
		t.Fatalf("status still published (%v) or awaiters not stopped (%d) after a failed build", terminalOutboxChecks(), awaiters.stopped.Load())
	}
}

// A composition failure is reported with its cause, not as a part that is merely
// missing.
func TestAFailedTerminalCompositionRefusesWithItsCause(t *testing.T) {
	withEpochWiring(t, epochWiringParts{
		Claims:       func(*lease.Manager, *lease.Handle) (epoch.Claims, error) { return fakeEpochClaims{}, nil },
		Observations: func() epoch.Observations { return epoch.Observations{} },
		Terminal:     failedTerminalPart(errors.New("terminal hooks: provider credentials: no secret store")),
	})
	if _, err := constructedEpoch(t)(context.Background(), testHandle); err == nil || !strings.Contains(err.Error(), "no secret store") {
		t.Fatalf("Epoch = %v, want the construction cause", err)
	}
}

// The heal's read accounting must not steal the scheduler's quota reset: with the
// real ledger, an EXPIRED window is still reported by ResetIfDue after a
// reservation (ReservePolls would have retired it and handed the reset to us).
func TestQuotaReserverAccountsReadsWithoutConsumingTheSchedulersReset(t *testing.T) {
	state := localscheduler.NewProviderQuotaState()
	past := time.Now().Add(-time.Minute)
	state.Record(apiv1.ProviderGitHub, 5, past)
	original := epochWiring
	t.Cleanup(func() { epochWiring = original })
	epochWiring.Quota = func() *localscheduler.ProviderQuotaState { return state }

	if !quotaReserver()(providers.ProviderGitHub) {
		t.Fatal("a window with budget refused a read")
	}
	if reset, due := state.ResetIfDue(apiv1.ProviderGitHub, time.Now()); !due || reset.ResetAt.IsZero() {
		t.Fatalf("ResetIfDue = %+v, %v after the heal's reservation; the scheduler lost its one-time reset", reset, due)
	}
	// Reads are accounted: an exhausted window refuses.
	live := localscheduler.NewProviderQuotaState()
	live.Record(apiv1.ProviderGitHub, 1, time.Now().Add(time.Hour))
	epochWiring.Quota = func() *localscheduler.ProviderQuotaState { return live }
	if !quotaReserver()(providers.ProviderGitHub) || quotaReserver()(providers.ProviderGitHub) {
		t.Fatal("a one-read window should allow exactly one read")
	}
	// No scheduler composed yet: unmetered.
	epochWiring.Quota = nil
	if !quotaReserver()(providers.ProviderGitHub) {
		t.Fatal("without a ledger reads must be allowed")
	}
}

// While the refused-cycle read is failing the answer is unknown, and unknown is not
// healthy; the last list still shows on the status page.
func TestAnUnreadableRefusedListFailsTheReadinessCheckClosed(t *testing.T) {
	t.Cleanup(func() { terminalStatus.Store(nil) })
	status := func() terminalcompose.Status {
		return terminalcompose.Status{Healthy: true, RefusedUnknown: true}
	}
	terminalStatus.Store(&status)
	if checks := terminalOutboxChecks(); checks["blockedCycleEscalation"] {
		t.Fatalf("checks = %v; want blockedCycleEscalation failing while the refused list is unreadable", checks)
	}
}

// A permanently refused escalation is a standing readiness failure and appears on
// the status page.
func TestARefusedCycleEscalationFailsItsReadinessCheck(t *testing.T) {
	t.Cleanup(func() { terminalStatus.Store(nil) })
	status := func() terminalcompose.Status {
		return terminalcompose.Status{Healthy: true, DeferredCycles: 1, RefusedCycles: []string{"github/acme/web#7"}}
	}
	terminalStatus.Store(&status)
	if checks := terminalOutboxChecks(); checks["blockedCycleEscalation"] || !checks["terminalOutbox"] {
		t.Fatalf("checks = %v; want only blockedCycleEscalation failing", checks)
	}
	if lag := currentTerminalOutboxLag(); lag == nil || lag.DeferredCycles != 1 || len(lag.RefusedCycles) != 1 {
		t.Fatalf("lag = %+v", lag)
	}
}
