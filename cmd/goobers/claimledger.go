package main

import (
	"os"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runtimewiring"
)

func claimFiles() scheduling.ClaimFiles {
	return scheduling.ClaimFiles{ClaimLock: withClaimLock, FileLock: withFileLock, BoundedLock: withBoundedFileLock, RunLock: withClaimLockForRun, SharedResolver: stageSharedClaimResolver, StartTelemetry: func(l instance.Layout) func() { return runtimewiring.StartCommandJournalTelemetry(l, os.Stderr) }}
}

var openStageClaimLedger = stageClaimLedger
var claimsPlaneSelected = scheduling.ClaimsPlaneSelected
var withClaimJournal = scheduling.WithClaimJournal
var claimContext = scheduling.ClaimContext

func stageClaimLedger(l instance.Layout, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return claimFiles().Stage(l, opts...)
}
func fileClaimLedger(l instance.Layout, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return claimFiles().File(l, opts...)
}

func stageClaimLedgerForRun(l instance.Layout, gaggle, runID string, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return claimFiles().StageForRun(l, gaggle, runID, opts...)
}
func claimLedgerJournal(l instance.Layout) (*journal.InstanceLog, func(), error) {
	return claimFiles().Journal(l)
}
func claimProviderReleaseLock(l instance.Layout) func(claimsclient.Key, func() error) error {
	return claimFiles().ProviderReleaseLock(l)
}
