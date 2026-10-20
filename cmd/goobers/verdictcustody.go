package main

import (
	"context"
	"fmt"
	"io"

	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/providers"
)

// Native status writers use the same durable publication claim as finding
// publication and final landing. Both local and plane clients share custody.
func withVerdictCustody(ctx context.Context, root string, repo providers.RepositoryRef, fn func(context.Context) error) error {
	return withVerdictPublicationCustody(ctx, root, repo, fn)
}

// publishCurrentVerdictStatus checks pins after acquiring the same custody as
// withdrawal, so a queued old publisher cannot restore a pass on a moved base.
func publishCurrentVerdictStatus(ctx context.Context, root string, provider remediationProvider, publisher any, req providers.PullRequestStatusRequest, stdout, stderr io.Writer) error {
	return withVerdictCustody(ctx, root, req.Repository, func(ctx context.Context) error {
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
