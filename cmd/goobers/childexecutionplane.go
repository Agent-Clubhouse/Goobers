package main

import (
	"context"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

type childExecutionPlane struct {
	service *daemonCredentialService
	claims  httpapi.ChildExecutionObserver
}

// List observes the parent's actual leases, retaining their original run and
// incarnation identities. It neither projects them as child-owned claims nor
// acquires or renews authority. The worker knows the parent from its signed
// contract; request values can only identify the worker's own child run.
func (p childExecutionPlane) List(ctx context.Context, req httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
	refuse := func() (httpapi.ClaimListResponse, error) {
		return httpapi.ClaimListResponse{}, httpapi.NewInterventionError(http.StatusForbidden, "child_execution_refused", "child execution authority is unavailable", nil)
	}
	if p.service == nil || p.claims == nil || !req.Execution || !req.IncludeHistory || req.Scope != httpapi.ClaimListScopeRun || req.Gaggle != "" || req.Provider != "" {
		return refuse()
	}
	a, err := p.service.childAttempt(ctx)
	if err != nil || req.RunID != a.contract.Identity.RunID || a.active(ctx) != nil || a.custody(ctx) != nil {
		return refuse()
	}
	// Reuse the accepted-source/current-policy lease, without resolving or
	// returning any credentials. Hold it through the observation and recheck
	// cancellation before returning so reload cannot widen the signed ceiling.
	_, lease, err := p.service.applyChildCredentialCeiling(ctx, pinnedStage{identity: a.contract.Identity}, a.contract.Stage)
	if err != nil {
		return refuse()
	}
	defer lease.release()
	parent := a.contract.Identity.Child.ParentRunID
	dir, err := p.service.layout.FindRunDir(parent)
	if err != nil {
		return refuse()
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return refuse()
	}
	id, err := reader.Identity()
	if err != nil || id.RunID != parent || id.Gaggle != a.contract.Identity.Gaggle || id.Child != nil {
		return refuse()
	}
	phase, err := reader.PhaseBounded(ctx)
	if err != nil || phase != journal.PhaseRunning {
		return refuse()
	}
	response, err := p.claims.List(ctx, httpapi.ClaimListRequest{RunID: parent, Scope: httpapi.ClaimListScopeRun, IncludeHistory: true, Execution: true})
	if err != nil {
		return refuse()
	}
	if response.ClaimVisibility != "local" && response.ClaimVisibility != "shared" {
		return refuse()
	}
	for _, entries := range [][]httpapi.ClaimEntry{response.Entries, response.History} {
		for _, entry := range entries {
			if entry.RunID != parent {
				return refuse()
			}
		}
	}
	if lease.finish(ctx) != nil {
		return refuse()
	}
	return response, nil
}
