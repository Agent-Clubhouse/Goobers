package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// claimledger.go is where every ledger-touching CLI stage gets its
// claimsclient.Ledger (finding 002 C1). Two constructions:
//
//   - stageClaimLedger: the STAGE seam. The plane backend when
//     GOOBERS_CLAIMS_ENDPOINT (+ the claims bearer) is in the stage's
//     environment — a stage pod — else the file backend below. Fail closed in
//     between: an endpoint without a bearer is an error, never a silent fall
//     through to a ledger file the pod does not have.
//   - fileClaimLedger / heldClaimLedger: the instance's own claims.json under
//     the same withClaimLock discipline, operation labels, timeout and
//     slow-lock journaling providercmd.go has always applied. The daemon's own
//     paths (terminal cleanup, interventions) use these directly and never
//     select by environment — the daemon is the writer, not a client.
//
// Nothing about the file path changes for a type-1/type-2 instance: the lock
// closure IS withClaimLock, the ledger open IS localscheduler.OpenClaimLedger,
// and Locked's fn runs inside exactly the critical section the seam held
// before this file existed.

// openStageClaimLedger is the stage seam, swappable in tests.
var openStageClaimLedger = stageClaimLedger

// stageClaimLedger selects a ledger for a stage CLI process from its
// environment.
func stageClaimLedger(l instance.Layout, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return claimsclient.Select(os.Getenv, func() (claimsclient.Ledger, error) {
		return fileClaimLedger(l, opts...)
	})
}

// claimLedgerJournal opens the instance log a file-backed claimant journals
// claim transitions to, or nothing when the stage is on the plane — the
// daemon's own ledger journals plane-driven transitions, and a pod has no
// instance log to open. The returned close is nil-safe.
func claimLedgerJournal(l instance.Layout) (*journal.InstanceLog, func(), error) {
	if claimsPlaneSelected() {
		return nil, func() {}, nil
	}
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		return nil, func() {}, fmt.Errorf("open instance log: %w", err)
	}
	return log, func() { _ = log.Close() }, nil
}

// claimsPlaneSelected reports whether the stage's environment names the
// claims plane — the same predicate Select applies, read here so a seam can
// skip instance-root side work (an instance-log open) the plane owns.
func claimsPlaneSelected() bool {
	return os.Getenv(claimsclient.EnvEndpoint) != ""
}

// withClaimJournal appends WithInstanceLog(log) when log is non-nil.
func withClaimJournal(log *journal.InstanceLog, opts ...localscheduler.LedgerOption) []localscheduler.LedgerOption {
	if log == nil {
		return opts
	}
	return append(opts, localscheduler.WithInstanceLog(log))
}

// fileClaimLedger is the instance's ledger under withClaimLock — one flock
// acquisition per Locked section, labelled by the caller.
func fileClaimLedger(l instance.Layout, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	return claimsclient.NewFile(claimsclient.FileConfig{
		LedgerPath: filepath.Join(l.SchedulerDir(), claimLedgerFileName),
		Lock: func(operation string, fn func() error) error {
			return withClaimLock(lockPath, operation, fn)
		},
		MergeLock: func(fn func() error) error {
			return withFileLock(filepath.Join(l.SchedulerDir(), mergeLockFileName), fn)
		},
		Options: opts,
	})
}

// fileClaimLedgerForRun is fileClaimLedger with withClaimLockForRun's lock
// (the run-attributed lock-event context the run-lifecycle releases use).
func fileClaimLedgerForRun(l instance.Layout, gaggle, runID string, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	return claimsclient.NewFile(claimsclient.FileConfig{
		LedgerPath: filepath.Join(l.SchedulerDir(), claimLedgerFileName),
		Lock: func(operation string, fn func() error) error {
			return withClaimLockForRun(lockPath, operation, gaggle, runID, fn)
		},
		Options: opts,
	})
}

// heldClaimLedger is the instance's ledger for a caller ALREADY inside its
// own withClaimLock*: no lock is taken (a second flock on the held path
// would wait on itself until the timeout), the ledger is opened fresh and
// operated on directly.
func heldClaimLedger(l instance.Layout, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return claimsclient.NewFile(claimsclient.FileConfig{
		LedgerPath: filepath.Join(l.SchedulerDir(), claimLedgerFileName),
		Options:    opts,
	})
}

// stageClaimLedgerForRun is the stage seam with the run-attributed file lock.
func stageClaimLedgerForRun(l instance.Layout, gaggle, runID string, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return claimsclient.Select(os.Getenv, func() (claimsclient.Ledger, error) {
		return fileClaimLedgerForRun(l, gaggle, runID, opts...)
	})
}

// claimContext is the context a CLI seam hands the ledger when it has none of
// its own: the plane backend bounds each round trip itself, and the file
// backend ignores it.
func claimContext() context.Context { return context.Background() }
