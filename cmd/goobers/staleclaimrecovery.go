package main

import (
	"errors"
	"fmt"
	"os"
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
// inputs). Off the plane, run the sweep in-process exactly as before — but
// refuse loudly first if the resolved root is not an instance, so a future
// caller that reaches here without either a root or an endpoint gets a
// refusal naming the missing variable instead of a relative-path ENOENT.

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
	_, err = recoverClaims(l, nil, now, nil, nil)
	return err
}

// instanceRootPresent reports whether the resolved layout actually addresses
// an instance, using the same marker every other subcommand's "not an
// instance root" check uses.
func instanceRootPresent(l instance.Layout) bool {
	_, err := os.Stat(l.ConfigFile())
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
