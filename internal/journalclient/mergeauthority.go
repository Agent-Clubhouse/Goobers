package journalclient

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/apicontract"
)

// MergeAuthorityRequest asks whether the admitted stage still has a merge
// capability in current daemon configuration. It never materializes credentials.
type MergeAuthorityRequest struct {
	RunID      string `json:"runId"`
	Gaggle     string `json:"gaggle"`
	Stage      string `json:"stage"`
	Capability string `json:"capability"`
}

// MergeAuthorityResponse reports a current grant without exposing credential material.
type MergeAuthorityResponse struct {
	Allowed bool `json:"allowed"`
}

// RequireMergeAuthority refuses unless the daemon verifies the admitted stage
// still holds the requested merge capability in current operator configuration.
func (h *HTTP) RequireMergeAuthority(ctx context.Context, stage, capability string) error {
	scope, err := h.gaggle("")
	if err != nil {
		return err
	}
	var response MergeAuthorityResponse
	if err := h.post(ctx, apicontract.JournalMergeAuthorityPath, MergeAuthorityRequest{RunID: h.cfg.RunID, Gaggle: scope, Stage: stage, Capability: capability}, &response); err != nil {
		return err
	}
	if !response.Allowed {
		return errors.New("current merge authority refused")
	}
	return nil
}
