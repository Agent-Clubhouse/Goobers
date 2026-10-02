package main

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/providers"
)

func statusWorkItemLookup(root string, definitions *instance.ConfigSet) readservice.WorkItemLookup {
	return func(ctx context.Context, gaggle, itemID string) (providers.WorkItem, error) {
		for i := range definitions.Gaggles {
			configured := &definitions.Gaggles[i]
			if configured.Name != gaggle {
				continue
			}
			repo, err := backlogProviderRef(gaggle, configured.Spec.Project, configured.Spec.Backlog)
			if err != nil {
				return providers.WorkItem{}, err
			}
			provider, err := newProviderForStage(root, repo, true, withStageProviderCache(), withStageProviderConfiguredADOAuth())
			if err != nil {
				return providers.WorkItem{}, err
			}
			return provider.GetWorkItem(ctx, repo, itemID)
		}
		return providers.WorkItem{}, fmt.Errorf("gaggle %q is not configured", gaggle)
	}
}
