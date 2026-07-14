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
	provider := newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))

	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	l := layoutFor(root)
	var claim localscheduler.ClaimEntry
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	err = withClaimLock(lockPath, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		entry, ok := ledger.ForRun(runID)
		if !ok {
			return fmt.Errorf("no item claimed by run %s in the claim ledger", runID)
		}
		claim = entry
		return nil
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
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
