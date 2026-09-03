package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/providers"
)

type statusPullRequestReader interface {
	providers.Provider
	GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error)
}

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

func statusPullRequestLookup(
	root string,
	cfg *instance.Config,
	definitions *instance.ConfigSet,
	stores credentials.StoreResolver,
	registrar credentials.SecretRegistrar,
) readservice.PullRequestLookup {
	return func(ctx context.Context, gaggle, pullID string) (providers.PullRequestSummary, error) {
		for i := range definitions.Gaggles {
			configured := &definitions.Gaggles[i]
			if configured.Name != gaggle {
				continue
			}
			project := configured.Spec.Project
			repo := providers.RepositoryRef{
				Provider: providers.ProviderKind(project.Provider),
				Owner:    project.Owner,
				Project:  project.Project,
				Name:     project.Name,
				URL:      project.BaseURL,
			}
			options := []stageProviderOption{withStageProviderCache()}
			if repo.Provider == providers.ProviderGitHub {
				resolver, grants, resolveErr := buildCredentials(
					cfg,
					stores,
					project.Owner,
					project.Name,
					nil,
					registrar,
				)
				if resolveErr != nil {
					return providers.PullRequestSummary{}, resolveErr
				}
				var tokenRef string
				for i := len(grants) - 1; i >= 0; i-- {
					if grants[i].Capability == string(capability.GitHubPRWrite) {
						tokenRef = grants[i].Ref
						break
					}
				}
				if tokenRef == "" {
					return providers.PullRequestSummary{}, fmt.Errorf(
						"gaggle %q has no credential for %q",
						gaggle,
						capability.GitHubPRWrite,
					)
				}
				token, tokenErr := resolver.Resolve(ctx, tokenRef)
				if tokenErr != nil {
					return providers.PullRequestSummary{}, tokenErr
				}
				options = append(options, withStageProviderToken(token))
			}
			provider, err := newProviderForStageAs[statusPullRequestReader](root, repo, true, options...)
			if err != nil {
				return providers.PullRequestSummary{}, err
			}
			return provider.GetPullRequest(ctx, repo, pullID)
		}
		return providers.PullRequestSummary{}, fmt.Errorf("gaggle %q is not configured", gaggle)
	}
}
