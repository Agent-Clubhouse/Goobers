package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/providers"
)

// A live origin can legally re-enter a writable stage even after releasing its
// issue claim or removing its worktree between stages. Its journaled branch
// ownership therefore fences remediation independently of claim expiry.
// This lookup must run outside the claims lock: pods use the journal plane.
func filterLiveOriginPullRequests(ctx context.Context, reader journalclient.CrossRun, gaggle, currentRunID string, candidates []providers.PullRequestSummary) ([]providers.PullRequestSummary, []string, error) {
	available := make([]providers.PullRequestSummary, 0, len(candidates))
	var deferred []string
	for _, pr := range candidates {
		origin := runBranchOriginRunID(pr.Head)
		if origin == "" || origin == currentRunID {
			available = append(available, pr)
			continue
		}
		parts := strings.Split(pr.Head, "/")
		response, err := reader.BranchOwnership(ctx, journalclient.BranchOwnershipRequest{
			RunID: currentRunID, Gaggle: gaggle, TargetRunID: origin,
			Workflow: parts[len(parts)-2], Branch: pr.Head,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("inspect PR #%d origin ownership: %w", pr.Number, err)
		}
		if response.Owner == nil {
			reason := response.Reason
			if reason == "" {
				reason = "unknown-ownership"
			}
			deferred = append(deferred, fmt.Sprintf("PR #%d deferred: origin ownership for branch %s (run %s, workflow %s) is unverified: %s", pr.Number, pr.Head, origin, parts[len(parts)-2], reason))
			continue
		}
		if !journal.RunPhase(response.Owner.Phase).IsTerminal() {
			deferred = append(deferred, fmt.Sprintf("PR #%d deferred: branch %s belongs to live origin run %s (workflow %s)",
				pr.Number, pr.Head, response.Owner.RunID, response.Owner.Workflow))
			continue
		}
		// Only verified terminal ownership permits a sibling to take this
		// run-scoped branch. Missing or unreadable evidence is a deferral.
		available = append(available, pr)
	}
	return available, deferred, nil
}

func stageLiveOriginPullRequests(root, runID string, candidates []providers.PullRequestSummary) ([]providers.PullRequestSummary, []string, error) {
	// Most PR heads are not run-scoped. Do not require a journal-plane
	// connection when there is no origin to inspect.
	needsLookup := false
	for _, pr := range candidates {
		if origin := runBranchOriginRunID(pr.Head); origin != "" && origin != runID {
			needsLookup = true
			break
		}
	}
	if !needsLookup {
		return candidates, nil, nil
	}
	reader, err := stageCrossRunJournal(root, nil)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	return filterLiveOriginPullRequests(ctx, reader, providerGaggle(), runID, candidates)
}
