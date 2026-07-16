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

// issueCloseOutStatus resolves the "status" Task.Input to the WorkItemStatus
// this stage sets, defaulting to WorkItemStatusDone for backward
// compatibility with any workflow that never declares it. Issue #361/#355:
// under the merge-review loop, the work isn't done until the PR merges, so
// `implementation`'s workflow now declares status=in-review here instead —
// only `goobers post-merge` (run by merge-review, at the actual merge event)
// advances the issue to done.
func issueCloseOutStatus(raw string) (providers.WorkItemStatus, error) {
	switch providers.WorkItemStatus(raw) {
	case "":
		return providers.WorkItemStatusDone, nil
	case providers.WorkItemStatusDone, providers.WorkItemStatusInReview:
		return providers.WorkItemStatus(raw), nil
	default:
		return "", fmt.Errorf("unsupported status %q (want %q or %q)", raw, providers.WorkItemStatusDone, providers.WorkItemStatusInReview)
	}
}

func runIssueCloseOut(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("issue-close-out", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers issue-close-out [path]\n\n"+
			"Comment on the issue this run claimed, linking its PR, mark it done (or,\n"+
			"with status=in-review, mark it in-review without closing — issue #361:\n"+
			"the work isn't done until the PR merges), and release the claim ledger's\n"+
			"lease early (rather than waiting for it to expire). Exit codes: 0 = done,\n"+
			"1 = business error, 2 = usage/IO error.\n")
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

	status, err := issueCloseOutStatus(providerInput("status", ""))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	ctx := context.Background()
	head := providerInput("head", providers.BranchName(workflow, runID))
	base := providerInput("base", "main")
	pr, found, err := provider.FindPullRequestByBranch(ctx, repo, head, base)
	if err != nil {
		return failProviderStage(stderr, "find pull request", err, "")
	}
	comment := providerInput("comment", "")
	if comment == "" {
		switch {
		case !found:
			comment = "Implementation complete."
		case status == providers.WorkItemStatusInReview:
			comment = fmt.Sprintf("Implementation complete: %s is open for merge-review.", pr.URL)
		default:
			comment = fmt.Sprintf("Implemented in %s.", pr.URL)
		}
	}

	if _, err := provider.UpdateWorkItemStatus(ctx, providers.UpdateWorkItemStatusRequest{
		Repository: repo,
		ID:         claim.ItemID,
		Status:     status,
		Comment:    comment,
	}); err != nil {
		pf(stderr, "error: update work item status: %v\n", err)
		return 1
	}

	// Release the goobers:claimed label on the same event that releases the
	// ledger claim below (#414 design point 1), regardless of status — even
	// the in-review branch above releases the ledger claim unconditionally,
	// and UpdateWorkItemStatus only ever swaps goobers/status:-prefixed
	// labels, so without this the claim marker survived indefinitely and a
	// fresh eligibility query could see a completed (or in-review) item as
	// still "claimed" forever. Best-effort like the ClaimWorkItem marker on
	// the claim side (backlogquery.go): the durable ledger release below,
	// not this label, is what's actually authoritative for eligibility, so a
	// failed removal here leaves only a stale human-visible marker, not a
	// stuck item.
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   repo,
		ID:           claim.ItemID,
		RemoveLabels: []string{providers.LabelClaimed},
	}); err != nil {
		pf(stderr, "warning: release %s claim label: %v\n", claim.ItemID, err)
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

	if status == providers.WorkItemStatusInReview {
		pf(stdout, "marked %s in-review (open PR, awaiting merge-review)\n", claim.ItemID)
	} else {
		pf(stdout, "closed out %s\n", claim.ItemID)
	}
	return 0
}
