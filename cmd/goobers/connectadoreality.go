package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/providers"
)

// This counts open tag matches, not claims or final workflow eligibility.
// ADO's WIQL substring filters are rechecked exactly by ListWorkItems.
func adoSelectorCount(ctx context.Context, client connectADOSeeder, repo providers.RepositoryRef, selectors []string) (int, bool, error) {
	count := 0
	cursor := ""
	seen := map[string]bool{"": true}
	for range 10 {
		var page providers.ListWorkItemsPageInfo
		items, err := client.ListWorkItems(ctx, providers.ListWorkItemsRequest{
			Repository: repo, State: "open", Labels: selectors, Limit: 100, Cursor: cursor, PageInfo: &page,
		})
		if err != nil {
			return 0, false, err
		}
		// The provider expands filtered pages to 250 raw candidates before
		// applying exact tags and process-specific open/closed state mapping.
		if len(items) > 100 || page.CandidateCount > 250 {
			return 0, false, fmt.Errorf("ADO selector scan exceeded its page bounds")
		}
		count += len(items)
		if !page.HasNext {
			return count, true, nil
		}
		if seen[page.NextCursor] {
			return 0, false, fmt.Errorf("ADO selector scan did not advance")
		}
		cursor = page.NextCursor
		seen[cursor] = true
	}
	return count, false, nil
}

func connectReportADOSelectorReality(opts connectOptions, selectors []string, stdout, stderr io.Writer) {
	token := os.Getenv(opts.tokenEnv)
	if token == "" {
		return
	}
	out := stdout
	if opts.json {
		out = stderr
	}
	repo, err := connectADOSeedRepository(opts)
	count, complete := 0, false
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), stubSampleProviderTimeout)
		defer cancel()
		count, complete, err = adoSelectorCount(ctx, newConnectADOSeeder(repo, token), repo, selectors)
	}
	if err != nil {
		pf(out, "note: Azure Boards selector matches unknown: %s\n", scrubRepositoryError(err, token))
		return
	}
	qualifier := ""
	if !complete {
		qualifier = "at least "
	}
	pf(out, "note: Azure Boards %s/%s: %s%d open work items match selector tags (not full workflow eligibility)\n", repo.Owner, repo.Project, qualifier, count)
	if !complete {
		pf(out, "note: selector scan reached its bound; the candidate pool is incomplete, not empty\n")
	} else if count == 0 {
		pf(out, "note: %s no open tag matches; use --seed or apply selector tags to an appropriate Boards task\n", connectSelectorRealityCode)
	}
}
