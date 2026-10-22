package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

const (
	claimLockOperationAPIAcquire = "api.claims.acquire"
	claimLockOperationAPIRenew   = "api.claims.renew"
	claimLockOperationAPIRelease = "api.claims.release"
	claimLockOperationAPISettle  = "api.claims.settle"
	claimLockOperationAPIList    = "api.claims.list"
	maxTriggerDedupeRecords      = planes.MaxTriggerDedupeRecords
)

type daemonClaimService struct {
	*planes.ClaimService
	layout        instance.Layout
	log           *journal.InstanceLog
	shared        claimsclient.SharedClaimResolver
	markerRelease func(context.Context, localscheduler.ClaimEntry, claimrelease.Reason) error
	mergeFlock    *mergeFlockState
	stopMergeHook func()
	recover       func(time.Time) ([]localscheduler.ClaimEntry, error)
}

func newDaemonClaimService(layout instance.Layout, log *journal.InstanceLog, recover func(time.Time) ([]localscheduler.ClaimEntry, error)) *daemonClaimService {
	service := &daemonClaimService{
		layout: layout, log: log,
		mergeFlock: newMergeFlockState(filepath.Join(layout.SchedulerDir(), mergeLockFileName)),
	}
	service.stopMergeHook = service.registerMergeLedgerObserver()
	if recover != nil {
		recover = func(original func(time.Time) ([]localscheduler.ClaimEntry, error)) func(time.Time) ([]localscheduler.ClaimEntry, error) {
			return func(now time.Time) ([]localscheduler.ClaimEntry, error) {
				released, recoverErr := original(now)
				return released, errors.Join(recoverErr, service.refreshMergeFlock())
			}
		}(recover)
	}
	service.recover = recover
	service.ClaimService = planes.NewClaimService(service, log, recover)
	return service
}

func (s *daemonClaimService) close() {
	if s.stopMergeHook != nil {
		s.stopMergeHook()
	}
	s.mergeFlock.close()
}

func (s *daemonClaimService) lockPath() string {
	return filepath.Join(s.layout.SchedulerDir(), claimLockFileName)
}

func (s *daemonClaimService) ledgerPath() string {
	return filepath.Join(s.layout.SchedulerDir(), claimLedgerFileName)
}

func (s *daemonClaimService) DefaultClaimLease() time.Duration { return DefaultClaimLease }
func (s *daemonClaimService) IsMergeLockRequest(request httpapi.ClaimRequest) bool {
	return isMergeLockRequest(request)
}
func (s *daemonClaimService) IsJournaledClaimsLockTimeout(err error) bool {
	return isJournaledClaimsLockTimeout(err)
}
func (s *daemonClaimService) WithLedger(operation string, request httpapi.ClaimRequest, fn func(*localscheduler.ClaimLedger) error) error {
	return s.withLedger(operation, request, fn)
}
func (s *daemonClaimService) CoordinatedLedger(operation string, request httpapi.ClaimRequest) (*claimsclient.File, error) {
	return s.coordinatedLedger(operation, request)
}
func (s *daemonClaimService) AcquireMergeLock(ctx context.Context, request httpapi.ClaimRequest, lease time.Duration) (httpapi.ClaimResponse, error) {
	return s.acquireMergeLock(ctx, request, lease)
}
func (s *daemonClaimService) RenewMergeLock(ctx context.Context, request httpapi.ClaimRequest, lease time.Duration) (httpapi.ClaimResponse, error) {
	return s.renewMergeLock(ctx, request, lease)
}
func (s *daemonClaimService) RenewSharedClaim(ctx context.Context, request httpapi.ClaimRequest, entry localscheduler.ClaimEntry, lease time.Duration) (httpapi.ClaimResponse, error) {
	return s.renewSharedClaim(ctx, request, entry, lease)
}
func (s *daemonClaimService) CurrentAdmissionResponse(ctx context.Context, client *claimsclient.File, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	return s.currentAdmissionResponse(ctx, client, request)
}
func (s *daemonClaimService) ReleaseProviderClaim(ctx context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	return s.releaseProviderClaim(ctx, request)
}
func (s *daemonClaimService) ExecutionClaimVisibility(ctx context.Context, runID string) (string, error) {
	return s.executionClaimVisibility(ctx, runID)
}
func (s *daemonClaimService) ExecutionClaimSnapshot(ctx context.Context, runID, mode string) (httpapi.ClaimListResponse, error) {
	return s.executionClaimSnapshot(ctx, runID, mode)
}
func (s *daemonClaimService) RunBelongsToGaggle(gaggle, runID string) bool {
	return runBelongsToGaggle(s.layout, gaggle, runID)
}
func (s *daemonClaimService) runBelongsToGaggle(gaggle, runID string) bool {
	return s.RunBelongsToGaggle(gaggle, runID)
}
func (s *daemonClaimService) journalRefusal(request httpapi.ClaimRequest, holder string) {
	s.JournalRefusal(request, holder)
}

func (s *daemonClaimService) withLedger(operation string, request httpapi.ClaimRequest, fn func(*localscheduler.ClaimLedger) error) error {
	return withClaimLockForRun(s.lockPath(), operation, request.Gaggle, request.RunID, func() error {
		opts := []localscheduler.LedgerOption{}
		if s.log != nil {
			opts = append(opts, localscheduler.WithInstanceLog(s.log))
		}
		ledger, err := localscheduler.OpenClaimLedger(s.ledgerPath(), opts...)
		if err != nil {
			return err
		}
		return fn(ledger)
	})
}

func claimKey(request httpapi.ClaimRequest) localscheduler.ClaimKey {
	return planes.ClaimKey(request)
}

func plainPathElement(value string) bool { return planes.PlainPathElement(value) }
func claimEntriesWire(entries []localscheduler.ClaimEntry) []httpapi.ClaimEntry {
	return planes.ClaimEntriesWire(entries)
}

type daemonTriggerService = planes.TriggerService

func newDaemonTriggerService() *daemonTriggerService { return planes.NewTriggerService() }
