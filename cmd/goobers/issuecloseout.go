package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func runIssueCloseOut(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("issue-close-out", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers issue-close-out [path]\n\n"+
			"Comment on the issue this run claimed, linking its PR, mark it done, and\n"+
			"release the claim ledger's lease early (rather than waiting for it to\n"+
			"expire). Exit codes: 0 = done, 1 = business error, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token)

	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	l := layoutFor(root)
	var claim localscheduler.ClaimEntry
	var claimHeld bool
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	err = withClaimLock(lockPath, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		entry, ok := ledger.ForRun(runID)
		if ok {
			claim = entry
			claimHeld = true
		}
		return nil
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if !claimHeld {
		// Resume-idempotency (#241): close-out RELEASES the claim as its very
		// last step, so an absent ledger entry means a prior attempt of this
		// stage already ran through the comment + mark-done + release. A crash
		// after the release but before stage.finished is journaled would
		// otherwise re-run close-out here, find no live claim, and fail the run
		// at its final stage after all real work succeeded. Treat an
		// already-released claim as done and succeed as a no-op so the run
		// terminates completed.
		pf(stdout, "run %s: claim already released by a prior close-out; nothing to do\n", runID)
		return 0
	}

	ctx := context.Background()
	head := providerInput("head", providers.BranchName(workflow, runID))
	base := providerInput("base", "main")
	pr, found, err := provider.FindPullRequestByBranch(ctx, repo, head, base)
	if err != nil {
		pf(stderr, "error: find pull request: %v\n", err)
		return 1
	}
	comment := providerInput("comment", "")
	if comment == "" {
		if found {
			comment = fmt.Sprintf("Implemented in %s.", pr.URL)
		} else {
			comment = "Implementation complete."
		}
	}

	if _, err := provider.UpdateWorkItemStatus(ctx, providers.UpdateWorkItemStatusRequest{
		Repository: repo,
		ID:         claim.ItemID,
		Status:     providers.WorkItemStatusDone,
		Comment:    comment,
	}); err != nil {
		pf(stderr, "error: update work item status: %v\n", err)
		return 1
	}

	// Release the lease now rather than waiting for it to expire — the run
	// is finished with this item, and RecoverExpired's periodic sweep
	// (goobers up, #131) should not have to reclaim it later.
	err = withClaimLock(lockPath, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		return ledger.Release(claim.ItemID, runID)
	})
	if err != nil {
		pf(stderr, "warning: release claim %s: %v\n", claim.ItemID, err)
	}

	pf(stdout, "closed out %s\n", claim.ItemID)
	return 0
}
