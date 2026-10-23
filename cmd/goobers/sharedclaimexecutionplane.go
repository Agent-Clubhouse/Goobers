package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// The ledger is atomically replaced. Execution needs a complete durable
// snapshot, not the write lock that can be held across remote lease renewal.
// Waiting for that lock would make a healthy renewal look like a stalled
// observer. Authentication and own-run policy checks remain in List.
func (s *daemonClaimService) executionClaimSnapshot(ctx context.Context, runID, mode string) (httpapi.ClaimListResponse, error) {
	if err := ctx.Err(); err != nil {
		return httpapi.ClaimListResponse{}, err
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(s.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		return httpapi.ClaimListResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return httpapi.ClaimListResponse{}, err
	}
	return httpapi.ClaimListResponse{ClaimVisibility: mode, ObservedAt: time.Now(),
		Entries: claimEntriesWire(ledger.ForRunAll(runID)), History: claimEntriesWire(ledger.HistoryForRun(runID))}, nil
}

func (s *daemonClaimService) executionClaimVisibility(ctx context.Context, runID string) (string, error) {
	directory, err := runDirFor(s.layout, runID)
	if err != nil {
		return "", err
	}
	reader, err := journal.OpenReadOnly(directory)
	if err != nil {
		return "", err
	}
	identity, err := reader.Identity()
	if err != nil {
		return "", err
	}
	if identity.RunID != runID {
		return "", fmt.Errorf("execution requires the owning run identity")
	}
	mode, err := localExecutionClaimVisibility(s.layout, apiv1.InvocationEnvelope{RunID: runID, Gaggle: identity.Gaggle, WorkflowID: identity.Workflow})
	if err != nil || mode != "shared" {
		return mode, err
	}
	phase, err := reader.PhaseBounded(ctx)
	if err != nil {
		return "", err
	}
	if phase.IsTerminal() {
		return "", claimsclient.ErrSharedExecutionExpired
	}
	return mode, nil
}
