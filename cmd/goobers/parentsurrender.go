package main

import (
	"context"
	"net/http"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
)

// parentSurrenderPlane owns writes only; parent tokens never gain worker read access.
type parentSurrenderPlane struct {
	store   dispatcher.SurrenderPlane
	service *daemonCredentialService
}

func (p parentSurrenderPlane) Put(ctx context.Context, run, stage string, attempt int, data []byte) error {
	refuse := func() error {
		return httpapi.NewInterventionError(http.StatusForbidden, "parent_surrender_refused", "parent surrender requires exact unresolved physical custody", nil)
	}
	if p.store == nil || p.service == nil {
		return refuse()
	}
	a, err := p.service.parentAttempt(ctx)
	if err != nil || run != a.contract.Identity.RunID || stage != a.contract.Stage || attempt != a.contract.PodAttempt || a.custody(ctx) != nil {
		return refuse()
	}
	if err = validateContainedSurrender(ctx, a.contract, a.digest, a.blobs, false, data); err != nil {
		return httpapi.NewInterventionError(http.StatusBadRequest, "parent_surrender_invalid", "parent result exceeds its contract or references unavailable attempt artifacts", nil)
	}
	return p.store.Put(ctx, run, stage, attempt, data)
}
