package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/providers"
)

// withVerdictCustody shares merge-pr's repository window with native verdict
// writers. The owning gaggle must run at most one such stage per run, and all
// writers of this status context must participate. Provider HTTP completion
// after a lost lease is not fenced by the forge's commit-status API.
func withVerdictCustody(ctx context.Context, root string, repo providers.RepositoryRef, fn func(context.Context) error) error {
	ledger, err := openStageClaimLedger(layoutFor(root))
	if err != nil {
		return err
	}
	return ledger.MergeLock(ctx, claimsclient.MergeLock{
		Key:   claimsclient.MergeLockKey(providerGaggle(), string(repo.Provider), repo.Owner, repo.Name),
		RunID: os.Getenv(executor.RunIDEnvVar), Workflow: os.Getenv(executor.WorkflowEnvVar),
	}, fn)
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
