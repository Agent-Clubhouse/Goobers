package main

import (
	"context"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
)

type parentExecutionPlane struct {
	service *daemonCredentialService
	claims  httpapi.ChildExecutionObserver
}

// A contained parent observes only its own original leases. The physical
// contract and current policy must remain live for every observation; this
// interface grants no claim acquisition, renewal, release or recovery.
func (p parentExecutionPlane) List(ctx context.Context, req httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
	refuse := func() (httpapi.ClaimListResponse, error) {
		return httpapi.ClaimListResponse{}, httpapi.NewInterventionError(http.StatusForbidden, "parent_execution_refused", "parent execution authority is unavailable", nil)
	}
	if p.service == nil || p.claims == nil || !req.Execution || !req.IncludeHistory || req.Scope != httpapi.ClaimListScopeRun || req.Gaggle != "" || req.Provider != "" {
		return refuse()
	}
	a, err := p.service.parentAttempt(ctx)
	if err != nil || req.RunID != a.contract.Identity.RunID {
		return refuse()
	}
	_, lease, err := p.service.applyParentCredentialCeiling(ctx, pinnedStage{identity: a.contract.Identity}, []string{a.contract.Stage})
	if err != nil {
		return refuse()
	}
	defer lease.release()
	response, err := p.claims.List(ctx, httpapi.ClaimListRequest{RunID: req.RunID, Scope: httpapi.ClaimListScopeRun, Execution: true, IncludeHistory: true})
	if err != nil || !childExecutionResponseMatches(response, req.RunID) || lease.finish(ctx) != nil {
		return refuse()
	}
	return response, nil
}
