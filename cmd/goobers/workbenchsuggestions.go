package main

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workbenchservice"
	"github.com/goobers/goobers/internal/workbenchsuggestions"
)

func (u *upSession) installWorkbenchSuggestions(proposals *workbenchservice.ProposalService) {
	service := &workbenchservice.SuggestionService{Proposals: proposals, OpenRun: u.openWorkbenchSuggestionRun}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithWorkbenchSuggestions(service))
}
func (u *upSession) openWorkbenchSuggestionRun(ctx context.Context, gaggle, runID string) (*journal.Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if gaggle == "" || !apiv1.ValidRunID(runID) {
		return nil, workbenchsuggestions.ErrArtifact
	}
	dir, err := u.l.FindRunDir(runID)
	if err != nil {
		return nil, workbenchsuggestions.ErrArtifact
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return nil, workbenchsuggestions.ErrArtifact
	}
	id, err := reader.Identity()
	if err != nil || id.RunID != runID || id.Gaggle != gaggle {
		return nil, workbenchsuggestions.ErrArtifact
	}
	return reader, ctx.Err()
}
