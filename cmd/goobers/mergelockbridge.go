package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

const claimLockOperationAPIMerge = "api.claims.merge"

type mergeFlockState struct {
	mu           sync.Mutex
	path         string
	handle       *platformlock.Handle
	liveLeases   int
	reservations int
}

// A daemon crash releases the kernel flock before its durable plane leases.
// Startup restores the flock before serving the claims plane. Until that
// succeeds, the daemon stays unavailable rather than admitting a local merge;
// an already-running pod may continue until a renewal is definitively refused,
// because transport errors during the restart are treated as transient.
func newMergeFlockState(path string) *mergeFlockState {
	return &mergeFlockState{path: path}
}

func isMergeLockRequest(request httpapi.ClaimRequest) bool {
	return strings.HasPrefix(request.ItemID, claimsclient.MergeLockItemPrefix) &&
		!localscheduler.IsPublicationClaimKey(request.ItemID)
}

func liveMergeLeaseCount(entries []localscheduler.ClaimEntry, now time.Time) int {
	count := 0
	for _, entry := range entries {
		if !entry.IsPublicationClaim() &&
			strings.HasPrefix(entry.ExternalID, claimsclient.MergeLockItemPrefix) &&
			entry.ExpiresAt.After(now) {
			count++
		}
	}
	return count
}

func (s *mergeFlockState) reserve() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle != nil {
		s.reservations++
		return true, nil
	}
	handle, err := platformlock.TryAcquire(s.path)
	if errors.Is(err, platformlock.ErrHeld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = handle.Announce(claimLockOperationAPIMerge)
	s.handle = handle
	s.reservations++
	return true, nil
}

func (s *mergeFlockState) reconcile(entries []localscheduler.ClaimEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveLeases = liveMergeLeaseCount(entries, time.Now())
	if s.liveLeases > 0 {
		if s.handle == nil {
			handle, err := platformlock.TryAcquire(s.path)
			if err == nil {
				_ = handle.Announce(claimLockOperationAPIMerge)
				s.handle = handle
			}
		}
		return
	}
	s.releaseIfIdle()
}

func (s *mergeFlockState) restore(entries []localscheduler.ClaimEntry, timeout time.Duration) error {
	s.mu.Lock()
	s.liveLeases = liveMergeLeaseCount(entries, time.Now())
	if s.liveLeases == 0 {
		s.releaseIfIdle()
		s.mu.Unlock()
		return nil
	}
	if s.handle != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	handle, err := acquireClaimLock(s.path, claimLockOperationAPIMerge, timeout, time.Now())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == nil && s.liveLeases > 0 {
		s.handle = handle
		return nil
	}
	_ = handle.Release()
	return nil
}

func (s *mergeFlockState) finishReservation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservations > 0 {
		s.reservations--
	}
	s.releaseIfIdle()
}

func (s *mergeFlockState) releaseIfIdle() {
	if s.liveLeases == 0 && s.reservations == 0 && s.handle != nil {
		_ = s.handle.Release()
		s.handle = nil
	}
}

func (s *mergeFlockState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle != nil {
		_ = s.handle.Release()
		s.handle = nil
	}
	s.liveLeases = 0
	s.reservations = 0
}

func (s *daemonClaimService) initializeMergeLock() error {
	var entries []localscheduler.ClaimEntry
	err := s.withLedger(claimLockOperationAPIMerge, httpapi.ClaimRequest{}, func(ledger *localscheduler.ClaimLedger) error {
		entries = ledger.Snapshot()
		return nil
	})
	if err != nil {
		return err
	}
	timeout, timeoutErr := claimsLockTimeoutForPath(s.mergeFlock.path)
	if timeoutErr != nil || timeout <= 0 {
		timeout = instance.DefaultClaimsLockTimeout
	}
	if err := s.mergeFlock.restore(entries, timeout); err != nil {
		if errors.Is(err, platformlock.ErrHeld) {
			return errors.New("acquire merge exclusion for live plane leases: merge.lock is held by a local merge")
		}
		return fmt.Errorf("acquire merge exclusion for live plane leases: %w", err)
	}
	return nil
}

func (s *daemonClaimService) acquireMergeLock(
	ctx context.Context,
	request httpapi.ClaimRequest,
	lease time.Duration,
) (httpapi.ClaimResponse, error) {
	held, err := s.mergeFlock.reserve()
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if !held {
		return httpapi.ClaimResponse{Ok: false, Holder: "local merge-pr"}, nil
	}
	defer s.mergeFlock.finishReservation()
	var response httpapi.ClaimResponse
	err = s.withLedger(claimLockOperationAPIAcquire, request, func(ledger *localscheduler.ClaimLedger) error {
		defer func() {
			s.mergeFlock.reconcile(ledger.Snapshot())
		}()
		client, err := s.coordinatedLedger(ledger)
		if err != nil {
			return err
		}
		ok, holder, err := client.ClaimScoped(ctx, claimKey(request), request.RunID, request.Workflow, lease)
		if err != nil {
			return err
		}
		if !ok {
			response = httpapi.ClaimResponse{Ok: false, Holder: holder}
			s.journalRefusal(request, holder)
			return nil
		}
		entry, held := ledger.LookupScoped(claimKey(request))
		if !held || entry.RunID != request.RunID {
			return errors.New("acquired merge claim is no longer held")
		}
		expires := entry.ExpiresAt
		response = httpapi.ClaimResponse{Ok: true, ExpiresAt: &expires}
		return nil
	})
	return response, err
}

func (s *daemonClaimService) renewMergeLock(
	ctx context.Context,
	request httpapi.ClaimRequest,
	lease time.Duration,
) (httpapi.ClaimResponse, error) {
	held, err := s.mergeFlock.reserve()
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if !held {
		return httpapi.ClaimResponse{Ok: false, Holder: "local merge-pr"}, nil
	}
	defer s.mergeFlock.finishReservation()
	var response httpapi.ClaimResponse
	err = s.withLedger(claimLockOperationAPIRenew, request, func(ledger *localscheduler.ClaimLedger) error {
		defer func() {
			s.mergeFlock.reconcile(ledger.Snapshot())
		}()
		entry, held := ledger.LookupScoped(claimKey(request))
		if !held || entry.RunID != request.RunID || !entry.ExpiresAt.After(time.Now()) {
			if held {
				response.Holder = entry.RunID
			}
			return nil
		}
		if !entry.SharedDeadline.IsZero() {
			var renewErr error
			response, renewErr = s.renewSharedClaim(ctx, ledger, request, entry, lease)
			return renewErr
		}
		ok, err := ledger.RenewEntry(entry, lease)
		if err != nil || !ok {
			return err
		}
		current, held := ledger.LookupScoped(claimKey(request))
		if !held || current.RunID != request.RunID {
			return errors.New("renewed merge claim is no longer held")
		}
		expires := current.ExpiresAt
		response = httpapi.ClaimResponse{Ok: true, ExpiresAt: &expires}
		return nil
	})
	return response, err
}

func (s *daemonClaimService) registerMergeLedgerObserver() func() {
	return localscheduler.RegisterClaimMutationObserver(
		filepath.Join(s.layout.SchedulerDir(), claimLedgerFileName),
		s.mergeFlock.reconcile,
	)
}

func (s *daemonClaimService) refreshMergeFlock() error {
	return s.withLedger(claimLockOperationAPIMerge, httpapi.ClaimRequest{}, func(ledger *localscheduler.ClaimLedger) error {
		s.mergeFlock.reconcile(ledger.Snapshot())
		return nil
	})
}
