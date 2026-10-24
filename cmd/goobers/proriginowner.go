package main

import (
	"github.com/goobers/goobers/providers"
)

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
