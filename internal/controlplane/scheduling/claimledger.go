package scheduling

import (
	"context"
	"crypto/sha256"
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
//   - fileClaimLedger: the instance's own claims.json under
//     the same withClaimLock discipline, operation labels, timeout and
//     slow-lock journaling providercmd.go has always applied. The daemon's own
//     paths (terminal cleanup, interventions) use these directly and never
//     select by environment — the daemon is the writer, not a client.
//
// Nothing about the file path changes for a type-1/type-2 instance: the lock
// closure IS withClaimLock, the ledger open IS localscheduler.OpenClaimLedger,
// and Locked's fn runs inside exactly the critical section the seam held
// before this file existed.

// Stage selects the stage backend from its environment, failing closed on incomplete credentials.
//
// stageClaimLedger selects a ledger for a stage CLI process from its
// environment.
func (f ClaimFiles) Stage(l instance.Layout, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return claimsclient.Select(os.Getenv, func() (claimsclient.Ledger, error) {
		return f.FileWithResolver(l, f.SharedResolver(l), opts...)
	})
}

// Journal opens the local claimant journal unless the claims plane owns it.
//
// claimLedgerJournal opens the instance log a file-backed claimant journals
// claim transitions to, or nothing when the stage is on the plane — the
// daemon's own ledger journals plane-driven transitions, and a pod has no
// instance log to open. The returned close is nil-safe.
func (f ClaimFiles) Journal(l instance.Layout) (*journal.InstanceLog, func(), error) {
	if ClaimsPlaneSelected() {
		return nil, func() {}, nil
	}
	stopTelemetry := f.StartTelemetry(l)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		stopTelemetry()
		return nil, func() {}, fmt.Errorf("open instance log: %w", err)
	}
	return log, func() {
		_ = log.Close()
		stopTelemetry()
	}, nil
}

// ClaimsPlaneSelected reports whether the stage's environment names the
// claims plane — the same predicate Select applies, read here so a seam can
// skip instance-root side work (an instance-log open) the plane owns.
func ClaimsPlaneSelected() bool {
	return os.Getenv(claimsclient.EnvEndpoint) != ""
}

// WithClaimJournal appends WithInstanceLog(log) when log is non-nil.
func WithClaimJournal(log *journal.InstanceLog, opts ...localscheduler.LedgerOption) []localscheduler.LedgerOption {
	if log == nil {
		return opts
	}
	return append(opts, localscheduler.WithInstanceLog(log))
}

// File constructs the file backend under the existing lock discipline.
//
// fileClaimLedger is the instance's ledger under withClaimLock — one flock
// acquisition per Locked section, labelled by the caller.
func (f ClaimFiles) File(l instance.Layout, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return f.FileWithResolver(l, nil, opts...)
}

// FileWithResolver constructs a file ledger with shared-claim coordination.
func (f ClaimFiles) FileWithResolver(l instance.Layout, resolver claimsclient.SharedClaimResolver, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	lockPath := filepath.Join(l.SchedulerDir(), "claims.lock")
	return claimsclient.NewFile(claimsclient.FileConfig{
		Shared:     resolver,
		LedgerPath: filepath.Join(l.SchedulerDir(), "claims.json"),
		Lock: func(operation string, fn func() error) error {
			return f.ClaimLock(lockPath, operation, fn)
		},
		MergeLock: func(fn func() error) error {
			return f.FileLock(filepath.Join(l.SchedulerDir(), "merge.lock"), fn)
		},
		ProviderReleaseLock: f.ProviderReleaseLock(l),
		Options:             opts,
	})
}

// ProviderReleaseLock serializes provider releases for one claim key.
func (f ClaimFiles) ProviderReleaseLock(l instance.Layout) func(claimsclient.Key, func() error) error {
	return func(key claimsclient.Key, fn func() error) error {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%q/%q/%q", key.Gaggle, key.Provider, key.ExternalID)))
		path := filepath.Join(l.SchedulerDir(), fmt.Sprintf("claim-release-%x.lock", digest))
		return f.BoundedLock(path, "backlog.release.provider", fn)
	}
}

// FileForRunWithResolver constructs a ledger using run-attributed claim locks.
//
// fileClaimLedgerForRunWithResolver uses withClaimLockForRun's lock
// (the run-attributed lock-event context the run-lifecycle releases use).
func (f ClaimFiles) FileForRunWithResolver(l instance.Layout, gaggle, runID string, resolver claimsclient.SharedClaimResolver, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	lockPath := filepath.Join(l.SchedulerDir(), "claims.lock")
	return claimsclient.NewFile(claimsclient.FileConfig{
		Shared:              resolver,
		LedgerPath:          filepath.Join(l.SchedulerDir(), "claims.json"),
		ProviderReleaseLock: f.ProviderReleaseLock(l),
		Lock: func(operation string, fn func() error) error {
			return f.RunLock(lockPath, operation, gaggle, runID, fn)
		},
		Options: opts,
	})
}

// StageForRun selects the stage ledger with a run-attributed file fallback.
//
// stageClaimLedgerForRun is the stage seam with the run-attributed file lock.
func (f ClaimFiles) StageForRun(l instance.Layout, gaggle, runID string, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return claimsclient.Select(os.Getenv, func() (claimsclient.Ledger, error) {
		return f.FileForRunWithResolver(l, gaggle, runID, f.SharedResolver(l), opts...)
	})
}

// ClaimContext is the context a CLI seam hands the ledger when it has none of
// its own: the plane backend bounds each round trip itself, and the file
// backend ignores it.
func ClaimContext() context.Context { return context.Background() }

// ClaimFiles supplies daemon-owned locking, shared-claim resolution and telemetry.
// Selection and file construction are shared by all stage and daemon callers.
type ClaimFiles struct {
	ClaimLock      func(string, string, func() error) error
	FileLock       func(string, func() error) error
	BoundedLock    func(string, string, func() error) error
	RunLock        func(string, string, string, string, func() error) error
	SharedResolver func(instance.Layout) claimsclient.SharedClaimResolver
	StartTelemetry func(instance.Layout) func()
}
