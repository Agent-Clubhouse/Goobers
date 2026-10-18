package main

import (
	"context"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type parentJournalPlane struct {
	writer  httpapi.JournalService
	service *daemonCredentialService
}

func (p parentJournalPlane) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	if p.writer == nil || p.service == nil {
		return livejournal.EmitResponse{}, parentJournalRefusal()
	}
	a, err := p.service.parentAttempt(ctx)
	if err != nil || a.custody(ctx) != nil {
		return livejournal.EmitResponse{}, parentJournalRefusal()
	}
	// Reuse the strict observation-only projection, including full-batch
	// validation and contract-scoped retry/capture identities. Workers cannot
	// choose a branch; the signed physical contract supplies it below.
	req, err = childJournalRequest(a.contract, a.digest, req)
	if err != nil {
		return livejournal.EmitResponse{}, err
	}
	for _, op := range req.Ops {
		if op.Event != nil && op.Event.Type == journal.EventStageHeartbeat && a.active(ctx) != nil {
			return livejournal.EmitResponse{}, parentJournalRefusal()
		}
	}
	ctx = livejournal.WithRequestBlobStore(ctx, a.blobs)
	return p.writer.Emit(livejournal.WithRequestBranch(ctx, a.contract.ParentBranch), req)
}

func parentJournalRefusal() error {
	return httpapi.NewInterventionError(http.StatusForbidden, "parent_journal_refused", "parent journal emission requires exact physical custody and observation-only operations", nil)
}
