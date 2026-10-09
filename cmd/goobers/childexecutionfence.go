package main

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/claimsclient"
)

// A child always monitors family authority, including when the parent's claims
// are local. The request names only the child; the signed contract selects the
// parent whose original provider-acknowledged deadlines constrain execution.
func remoteChildExecutionFence(ctx context.Context, baseURL, token string, contract childpod.Contract) (context.Context, context.CancelFunc, error) {
	noop := func() {}
	if contract.Identity.Child == nil || contract.Identity.ValidateChildLineage() != nil || token == "" {
		return ctx, noop, fmt.Errorf("child execution requires signed parent lineage")
	}
	observer, err := claimsclient.NewExecutionObserver(claimsclient.HTTPConfig{BaseURL: baseURL, Token: token, RunID: contract.Identity.RunID})
	if err != nil {
		return ctx, noop, err
	}
	parent := contract.Identity.Child.ParentRunID
	mode := ""
	return claimsclient.StartExecutionFence(ctx, parent, func(ctx context.Context) (claimsclient.Listing, error) {
		current, listing, err := observer.ExecutionSnapshot(ctx)
		if err != nil {
			return listing, err
		}
		if mode != "" && current != mode {
			return listing, fmt.Errorf("parent execution policy changed")
		}
		mode = current
		for _, entries := range [][]claimsclient.Entry{listing.Entries, listing.History} {
			for _, entry := range entries {
				if entry.RunID != parent {
					return listing, fmt.Errorf("child observer returned foreign claim owner")
				}
			}
		}
		return listing, nil
	})
}
