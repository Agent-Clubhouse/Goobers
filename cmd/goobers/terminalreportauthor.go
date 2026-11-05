package main

import (
	"context"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/providers"
)

// OwnCommentMatcher resolves the identity on the same backlog connection used
// to publish the report. A code repository may use a different backlog provider.
func (c *escalationCommenter) OwnCommentMatcher(ctx context.Context, repository providers.RepositoryRef) (func(providers.Comment) bool, error) {
	if repository.Provider == providers.ProviderADO {
		backlog := backlogRepoRefForGaggle(c.layout, repository)
		if !providerconfig.BacklogOnOtherProvider(repository, backlog) {
			provider, err := newConfiguredADOProvider(c.layout.Root, repository)
			if err != nil {
				return nil, err
			}
			return gate.OwnCommentMatcher(ctx, provider, backlog)
		}
		repository = backlog
	}
	token, err := c.resolver.Resolve(ctx, repository.Owner+"/"+repository.Name)
	if err != nil {
		return nil, err
	}
	c.reg.Register([]byte(token))
	if repository.Provider == providers.ProviderGitea {
		provider, err := newGiteaProviderForStage(c.layout.Root, repository, token)
		if err != nil {
			return nil, err
		}
		return gate.OwnCommentMatcher(ctx, provider, repository)
	}
	provider := newEscalationPoster(token)
	if github, ok := provider.(*providers.GitHubProvider); ok {
		for _, option := range stageProviderConfiguredLogin(c.layout.Root, repository).options() {
			option(github)
		}
	}
	return gate.OwnCommentMatcher(ctx, provider, repository)
}
