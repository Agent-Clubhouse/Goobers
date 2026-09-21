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
type MergeAuthorityResponse struct {
	Allowed bool `json:"allowed"`
}

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
