package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

const claimLockOperationAPIMerge = "api.claims.merge"

type daemonMergeLock struct {
	key    localscheduler.ClaimKey
	runID  string
	handle *platformlock.Handle
}

func isMergeLockRequest(request httpapi.ClaimRequest) bool {
	return strings.HasPrefix(request.ItemID, claimsclient.MergeLockItemPrefix)
}

func (s *daemonClaimService) mergeLockPath() string {
	return filepath.Join(s.layout.SchedulerDir(), mergeLockFileName)
}

func (s *daemonClaimService) acquireMergeLock(
	ctx context.Context,
	request httpapi.ClaimRequest,
	lease time.Duration,
) (httpapi.ClaimResponse, error) {
	s.mergeAcquire.Lock()
	defer s.mergeAcquire.Unlock()

	key := claimKey(request)
	var response httpapi.ClaimResponse
	var needsFileLock bool
	err := s.withLedger(claimLockOperationAPIAcquire, request, func(ledger *localscheduler.ClaimLedger) error {
		entry, held := ledger.LookupScoped(key)
		if held && entry.ExpiresAt.After(time.Now()) {
			if entry.RunID != request.RunID {
				response = httpapi.ClaimResponse{Ok: false, Holder: entry.RunID}
				s.journalRefusal(request, entry.RunID)
				return nil
			}
			if s.ownsMergeLock(request.RunID, key) {
				return s.claimMergeLock(ctx, ledger, request, lease, &response)
			}
		}
		needsFileLock = true
		return nil
	})
	if err != nil || !needsFileLock {
		return response, err
	}

	s.releaseAnyMergeLock()
	handle, err := s.takeMergeFileLock(ctx)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = handle.Release()
		}
	}()

	err = s.withLedger(claimLockOperationAPIAcquire, request, func(ledger *localscheduler.ClaimLedger) error {
		return s.claimMergeLock(ctx, ledger, request, lease, &response)
	})
	if err != nil || !response.Ok {
		return response, err
	}
	s.setMergeLock(request.RunID, key, handle)
	keep = true
	return response, nil
}

func (s *daemonClaimService) claimMergeLock(
	ctx context.Context,
	ledger *localscheduler.ClaimLedger,
	request httpapi.ClaimRequest,
	lease time.Duration,
	response *httpapi.ClaimResponse,
) error {
	client, err := s.coordinatedLedger(ledger)
	if err != nil {
		return err
	}
	ok, holder, err := client.ClaimScoped(ctx, claimKey(request), request.RunID, request.Workflow, lease)
	if err != nil {
		return err
	}
	if !ok {
		*response = httpapi.ClaimResponse{Ok: false, Holder: holder}
		s.journalRefusal(request, holder)
		return nil
	}
	entry, held := ledger.LookupScoped(claimKey(request))
	if !held || entry.RunID != request.RunID {
		return errors.New("acquired merge claim is no longer held")
	}
	expires := entry.ExpiresAt
	*response = httpapi.ClaimResponse{Ok: true, ExpiresAt: &expires}
	return nil
}

func (s *daemonClaimService) renewMergeLock(
	ctx context.Context,
	request httpapi.ClaimRequest,
	lease time.Duration,
) (httpapi.ClaimResponse, error) {
	s.mergeAcquire.Lock()
	defer s.mergeAcquire.Unlock()

	key := claimKey(request)
	var response httpapi.ClaimResponse
	var needsFileLock bool
	err := s.withLedger(claimLockOperationAPIRenew, request, func(ledger *localscheduler.ClaimLedger) error {
		entry, held := ledger.LookupScoped(key)
		if !held || entry.RunID != request.RunID || !entry.ExpiresAt.After(time.Now()) {
			if held {
				response.Holder = entry.RunID
			}
			return nil
		}
		if !s.ownsMergeLock(request.RunID, key) {
			needsFileLock = true
			return nil
		}
		return s.renewMergeClaim(ledger, request, lease, &response)
	})
	if err != nil || !needsFileLock {
		if err == nil && !response.Ok {
			s.releaseMergeLock(request.RunID, key)
		}
		return response, err
	}

	s.releaseAnyMergeLock()
	handle, err := s.takeMergeFileLock(ctx)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = handle.Release()
		}
	}()
	err = s.withLedger(claimLockOperationAPIRenew, request, func(ledger *localscheduler.ClaimLedger) error {
		return s.renewMergeClaim(ledger, request, lease, &response)
	})
	if err != nil {
		s.setMergeLock(request.RunID, key, handle)
		keep = true
		return response, err
	}
	if !response.Ok {
		return response, err
	}
	s.setMergeLock(request.RunID, key, handle)
	keep = true
	return response, nil
}

func (s *daemonClaimService) renewMergeClaim(
	ledger *localscheduler.ClaimLedger,
	request httpapi.ClaimRequest,
	lease time.Duration,
	response *httpapi.ClaimResponse,
) error {
	entry, held := ledger.LookupScoped(claimKey(request))
	if !held || entry.RunID != request.RunID || !entry.ExpiresAt.After(time.Now()) {
		if held {
			response.Holder = entry.RunID
		}
		return nil
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
	*response = httpapi.ClaimResponse{Ok: true, ExpiresAt: &expires}
	return nil
}

func (s *daemonClaimService) takeMergeFileLock(ctx context.Context) (*platformlock.Handle, error) {
	timeout, err := claimsLockTimeoutForPath(s.mergeLockPath())
	if err != nil || timeout <= 0 {
		timeout = instance.DefaultClaimsLockTimeout
	}
	return acquireClaimLockContext(ctx, s.mergeLockPath(), claimLockOperationAPIMerge, timeout, time.Now())
}

func (s *daemonClaimService) ownsMergeLock(runID string, key localscheduler.ClaimKey) bool {
	s.mergeMu.Lock()
	defer s.mergeMu.Unlock()
	return s.mergeHeld != nil && s.mergeHeld.runID == runID && s.mergeHeld.key == key
}

func (s *daemonClaimService) setMergeLock(runID string, key localscheduler.ClaimKey, handle *platformlock.Handle) {
	s.mergeMu.Lock()
	defer s.mergeMu.Unlock()
	s.mergeHeld = &daemonMergeLock{key: key, runID: runID, handle: handle}
}

func (s *daemonClaimService) releaseMergeLock(runID string, key localscheduler.ClaimKey) {
	s.mergeMu.Lock()
	if s.mergeHeld == nil || s.mergeHeld.runID != runID || s.mergeHeld.key != key {
		s.mergeMu.Unlock()
		return
	}
	held := s.mergeHeld
	s.mergeHeld = nil
	s.mergeMu.Unlock()
	_ = held.handle.Release()
}

func (s *daemonClaimService) releaseMergeLockForRun(runID, gaggle, provider string) {
	s.mergeMu.Lock()
	if s.mergeHeld == nil || s.mergeHeld.runID != runID ||
		(gaggle != "" && (s.mergeHeld.key.Gaggle != gaggle || s.mergeHeld.key.Provider != provider)) {
		s.mergeMu.Unlock()
		return
	}
	held := s.mergeHeld
	s.mergeHeld = nil
	s.mergeMu.Unlock()
	_ = held.handle.Release()
}

func (s *daemonClaimService) releaseAnyMergeLock() {
	s.mergeMu.Lock()
	held := s.mergeHeld
	s.mergeHeld = nil
	s.mergeMu.Unlock()
	if held != nil {
		_ = held.handle.Release()
	}
}

func (s *daemonClaimService) recoverExpired(now time.Time) ([]localscheduler.ClaimEntry, error) {
	s.mergeAcquire.Lock()
	defer s.mergeAcquire.Unlock()
	released, err := s.recover(now)
	for _, entry := range released {
		if strings.HasPrefix(entry.ExternalID, claimsclient.MergeLockItemPrefix) {
			s.releaseMergeLock(entry.RunID, localscheduler.ClaimKey{
				Gaggle: entry.Gaggle, Provider: entry.Provider, ExternalID: entry.ExternalID,
			})
		}
	}
	return released, err
}
