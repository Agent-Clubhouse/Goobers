package main

import (
	"context"
	"fmt"
	"strings"

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
			project := configured.Spec.Project
			backlog := configured.Spec.Backlog
			repo := providers.RepositoryRef{
				Provider: providers.ProviderKind(backlog.Provider),
				Owner:    project.Owner,
				Project:  project.Project,
				Name:     project.Name,
				URL:      backlog.BaseURL,
			}
			switch repo.Provider {
			case providers.ProviderGitHub, providers.ProviderGitea:
				owner, name, ok := strings.Cut(backlog.Project, "/")
				if !ok || owner == "" || name == "" {
					return providers.WorkItem{}, fmt.Errorf(
						"gaggle %q backlog project %q must be owner/name",
						gaggle,
						backlog.Project,
					)
				}
				repo.Owner, repo.Name = owner, name
			case providers.ProviderADO:
				repo.Project = backlog.Project
			}
			provider, err := newProviderForStage(root, repo, true, withStageProviderCache())
			if err != nil {
				return providers.WorkItem{}, err
			}
			return provider.GetWorkItem(ctx, repo, itemID)
		}
		return providers.WorkItem{}, fmt.Errorf("gaggle %q is not configured", gaggle)
	}
}
