package providers

import (
	"context"
	"fmt"
	"net/http"
)

// postDirectMerge requires Gitea's direct-merge receipt. The merge handler
// returns 200 only after merging; 201 denotes scheduling auto-merge, which
// must not be credited as a completed landing. This caller does not request
// MergeWhenChecksSucceed or the manually-merged action.
// Contract: https://github.com/go-gitea/gitea/blob/main/routers/api/v1/repo/pull.go
func (p *GiteaProvider) postDirectMerge(ctx context.Context, endpoint string, body any) error {
	response, err := p.send(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	status := response.StatusCode
	// Preserve ordinary provider errors and close the response body on all
	// paths, including successful-but-not-confirmed response codes.
	if err := readJSONResponse(response, http.MethodPost, endpoint, nil); err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("gitea: direct merge was not confirmed (HTTP %d)", status)
	}
	return nil
}
