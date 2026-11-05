package main

import (
	"context"
	"fmt"
	"io"

	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/providers"
)

// publishCurrentVerdictStatus checks pins after acquiring the same custody as
// withdrawal, so a queued old publisher cannot restore a pass on a moved base.
func publishCurrentVerdictStatus(ctx context.Context, root string, provider remediationProvider, publisher any, req providers.PullRequestStatusRequest, stdout, stderr io.Writer) error {
	return withVerdictPublicationCustody(ctx, root, req.Repository, func(ctx context.Context) error {
		live, err := provider.GetPullRequest(ctx, req.Repository, req.PullID)
		if err != nil {
			return err
		}
		_, base, ok := prstatus.ParseVerdictDescription(req.Description)
		if !ok || live.HeadSHA != req.HeadSHA || live.BaseSHA != base {
			return fmt.Errorf("verdict status pins changed before publication; re-review the live pull request")
		}
		return publishVerdictStatus(ctx, publisher, req, false, stdout, stderr)
	})
}
