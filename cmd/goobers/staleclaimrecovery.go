package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
)

// staleclaimrecovery.go is the STAGE seam for the claim ledger's stale-claim
// sweep — expired leases plus leases whose owning run is already terminal
// (Goobers#4016).
//
// The sweep is the odd one out among the ledger operations a CLI stage
// performs. Acquire, renew, release and list are per-item primitives with an
// obvious plane shape, and claimledger.go's seam serves them. Recovery is not
// a primitive at all: it is a decision about EVERY lease in the instance,
// taken from three inputs that exist only in the daemon —
//
//   - terminality, resolved by reading the OWNING run's journal under the
//     instance root (claimHolderTerminal),
//   - the active-intervention set, which is in-memory daemon state and must
//     keep a held claim from being reaped,
//   - the restart-time recovery gate, which exists so a sweep never races the
//     renewal pass that re-establishes a live distributed run's leases across
//     a daemon restart.
//
// `backlog-query --reconcile` nonetheless needs the sweep to have happened
// before it inspects provider claim markers, so that a marker left by a dead
// claimant can be cleared. Before this seam it simply called recoverClaims
// in-process. That was correct while the stage only ever ran in the daemon's
// own process tree; the moment reconcile-backlog-metadata was actually
// dispatched onto a pod (unblocked by #3992), instance.Layout's root fell
// through providerStageRoot's last resort to ".", and the very first thing
// the sweep does — take the claims lock — became `open "scheduler/claims.lock":
// no such file or directory` against the pod's working directory. Every
// scheduled backlog-curation run failed on it.
//
// So: on the plane, ask the daemon to run ITS sweep (the one with all three
// inputs). Off the plane, ask the daemon a different way when a daemon owns
// this instance, and only run the sweep in-process when nothing else can
// (Goobers#4029) — refusing loudly first if the resolved root is not an
// instance, so a future caller that reaches here without either a root or an
// endpoint gets a refusal naming the missing variable instead of a
// relative-path ENOENT.
//
// GOOBERS#4029 — WHY THE FILE ARM IS NOT SIMPLY "SWEEP IN PROCESS".
//
// The first two of the three inputs above exist ONLY while a daemon is up:
// newRunInterventionService is constructed in up.go and nowhere else, and
// localscheduler.RecoveryGate is the in-memory latch that same process opens
// once its startup renewal rebuild has completed. That gives the seam a
// single, decidable predicate — DOES A DAEMON OWN THIS INSTANCE? — and it is
// exactly the predicate for whether the two missing inputs are missing at all:
//
//   - A daemon holds up.lock. Then interventions can be active and the gate
//     can be closed, and a stage passing nil for both would reap a lease the
//     daemon is deliberately holding. The sweep is DELEGATED to it, through
//     the same pending-claims channel `goobers claims release` already uses
//     when it finds a live daemon (claims.go). The daemon answers with its
//     own recoverExpiredClaims — the same closure the startup pass, the
//     five-minute ticker and the claims plane's /claims/recover all call, so
//     there is exactly one sweep implementation and three ways to ask for it.
//   - Nothing, or a MANUAL holder (`goobers run`/`goobers signal`, which take
//     the same lock for the length of the one-shot), owns it. Then no
//     intervention service exists to consult and no gate exists to respect:
//     nil and nil are the TRUTH, not an omission, and are precisely what
//     newOneShotClaimRecovery's documented nil-gate default and
//     buildSchedulerSetup's mode-1 setup reap already do on those paths. The
//     sweep runs in process, byte-identical to before.
//
// A stage is never the process that decides an intervention is inactive.

// recoverStageClaims is the seam, swappable in tests.
var recoverStageClaims = stageRecoverStaleClaims

// errStageClaimRecoveryRootless is the loud refusal replacing the silent
// relative-path fallback. Its message names both ways out on purpose: a pod
// is missing its claims endpoint, a standalone invocation is missing its
// root.
var errStageClaimRecoveryRootless = fmt.Errorf(
	"claim recovery needs either the claims plane (%s) or an instance root (%s, or a path argument); "+
		"neither is set, and a relative scheduler directory is not one",
	claimsclient.EnvEndpoint, executor.InstanceRootEnvVar)

// stageRecoverStaleClaims performs the pre-reconciliation stale-claim sweep
// from a stage process, on whichever side of the plane the stage is running.
func stageRecoverStaleClaims(l instance.Layout, now time.Time) error {
	ledger, err := openStageClaimLedger(l)
	if err != nil {
		return fmt.Errorf("open claim ledger: %w", err)
	}
	if recoverer, ok := ledger.(claimsclient.StaleRecoverer); ok {
		if _, err := recoverer.RecoverStale(claimContext()); err != nil {
			return fmt.Errorf("recover claims over the claims plane: %w", err)
		}
		return nil
	}
	if !instanceRootPresent(l) {
		return errStageClaimRecoveryRootless
	}
	daemonOwns, err := daemonOwnsInstance(l)
	if err != nil {
		return fmt.Errorf("inspect the daemon lock before recovering claims: %w", err)
	}
	if daemonOwns {
		return delegateStaleClaimRecovery(l)
	}
	_, err = recoverClaims(l, nil, now, nil, nil)
	return err
}

// daemonOwnsInstance reports whether a live `goobers up` holds this
// instance's lock — the durable, cross-process signal for "the in-memory
// intervention set and recovery gate exist, and belong to someone else". A
// MANUAL holder (`goobers run`/`signal`) is deliberately not a daemon here:
// those processes have no intervention service and pass no gate themselves,
// so a stage under one is in exactly the mode-1 shape the nil arguments
// describe.
func daemonOwnsInstance(l instance.Layout) (bool, error) {
	running, _, err := inspectDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	return running, err
}

// delegateStaleClaimRecovery asks the live daemon to run its own sweep and
// waits for the answer, over the pending-claims request/response channel
// claims.go already defines. Errors are surfaced rather than swallowed, for
// the same reason the plane arm above surfaces them: a stage that asked for a
// sweep and did not get one must say so, not proceed as though the ledger
// were fresh.
func delegateStaleClaimRecovery(l instance.Layout) error {
	requestID, err := writeClaimAdminRequest(l.SchedulerDir(), claimAdminRequest{
		Operation: claimAdminOperationRecover,
	})
	if err != nil {
		return fmt.Errorf("delegate claim recovery to the running daemon: %w", err)
	}
	resp, err := pollClaimAdminResponse(claimContext(), l.SchedulerDir(), requestID, claimAdminDelegationTimeout)
	if err != nil {
		return fmt.Errorf("delegate claim recovery to the running daemon: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("delegate claim recovery to the running daemon: %s", resp.Error)
	}
	return nil
}

// instanceRootPresent reports whether the resolved layout actually addresses
// an instance, using the same marker every other subcommand's "not an
// instance root" check uses.
func instanceRootPresent(l instance.Layout) bool {
	_, err := os.Stat(l.ConfigFile())
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
