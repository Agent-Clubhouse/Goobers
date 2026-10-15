package main

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// The terminal preparer (branch cleanup + run-abort labeling) runs OUTSIDE a
// provider-chain stage: it is the runner's own finalize hook, so it has no
// routed stage env and no providerToken. It therefore resolves both the
// repository it acts on and the backend that reaches it from instance config
// alone — cfg.Repos[0], the same repo buildTerminalBranchDelete has always
// used — rather than from a stage's routed RepositoryRef.
//
// Both entrypoints must preserve the configured provider. Dispatching terminal
// cleanup through another provider loses branch deletion and run-abort labels,
// breaking the terminal-state invariants those hooks enforce.

// terminalRepositoryRefForProject is the repository the terminal preparer acts
// on, carrying the repo's own declared provider kind. The kind is load-bearing
// beyond dispatch because it is stamped into branch-cleanup journal facts.
func terminalRepositoryRefForProject(cfg *instance.Config, project apiv1.RepoRef) providers.RepositoryRef {
	if cfg == nil || len(cfg.Repos) == 0 {
		return providers.RepositoryRef{}
	}
	repo := cfg.Repos[0]
	if project.Owner != "" && project.Name != "" {
		if configured, ok := configuredRepoForProject(cfg, project); ok {
			repo = configured
		} else {
			provider := providers.ProviderKind(project.Provider)
			if provider == "" {
				provider = providers.ProviderKind(repo.Provider)
			}
			if provider == "" {
				provider = providers.ProviderGitHub
			}
			return providers.RepositoryRef{Provider: provider, Owner: project.Owner, Project: project.Project, Name: project.Name}
		}
	}
	provider := providers.ProviderKind(repo.Provider)
	if provider == "" {
		provider = providers.ProviderGitHub
	}
	// Project is the Azure DevOps project the repository lives in (empty for
	// GitHub and Gitea); the ADO arm addresses the repository through it.
	return providers.RepositoryRef{
		Provider: provider,
		Owner:    repo.Owner,
		Project:  repo.Project,
		Name:     repo.Name,
	}
}

func terminalConfiguredRepo(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, error) {
	if cfg == nil || len(cfg.Repos) == 0 {
		return instance.RepoRef{}, fmt.Errorf("no repository configured for terminal provider")
	}
	if project.Owner == "" || project.Name == "" {
		return cfg.Repos[0], nil
	}
	if repo, ok := configuredRepoForProject(cfg, project); ok {
		return repo, nil
	}
	return instance.RepoRef{}, fmt.Errorf("terminal project repository %s/%s is not configured", project.Owner, project.Name)
}

// terminalGiteaBaseURLForProject returns the configured forge root for a Gitea
// terminal repo. Config validation requires baseUrl on every Gitea repo, so an
// empty value fails rather than degrading to a default host.
func terminalGiteaBaseURLForProject(cfg *instance.Config, project apiv1.RepoRef) (string, error) {
	repo, err := terminalConfiguredRepo(cfg, project)
	if err != nil {
		return "", err
	}
	if repo.BaseURL == "" {
		return "", fmt.Errorf("gitea repo %s/%s has no baseUrl configured", repo.Owner, repo.Name)
	}
	return repo.BaseURL, nil
}

// terminalADOArm builds a terminal hook's Azure DevOps backend from the
// terminal repository, the credential kind its token is sent as and the
// materialized capability token. A nil arm means the hook has no ADO backend.
type terminalADOArm[T any] func(repo providers.RepositoryRef, kind string, source providers.TokenSource) (T, error)

// terminalProviderForProject dispatches a terminal hook on the terminal
// repository's provider. adoScheme is the scheme stated for the capability
// the ADO arm's token was materialized for (adoCredentialSchemes), "" to use
// the repository's own.
func terminalProviderForProject[T any](
	cfg *instance.Config,
	project apiv1.RepoRef,
	source providers.TokenSource,
	github func(providers.TokenSource) T,
	gitea func(string, providers.TokenSource) T,
	ado terminalADOArm[T],
	adoScheme string,
	unsupported func(providers.ProviderKind) error,
) (T, error) {
	repo := terminalRepositoryRefForProject(cfg, project)
	switch repo.Provider {
	case providers.ProviderADO:
		if ado == nil {
			break
		}
		kind, err := terminalADOCredentialKind(cfg, project, adoScheme)
		if err != nil {
			var zero T
			return zero, err
		}
		return ado(repo, kind, source)
	case providers.ProviderGitea:
		baseURL, err := terminalGiteaBaseURLForProject(cfg, project)
		if err != nil {
			var zero T
			return zero, err
		}
		return gitea(baseURL, source), nil
	case providers.ProviderGitHub:
		return github(source), nil
	}
	var zero T
	return zero, unsupported(repo.Provider)
}

// terminalADOCredentialKind is the Azure DevOps credential kind the terminal
// repository's capability token is sent as: stated, the scheme of the
// capability's own source (a credentials: entry's PAT is "basic", #5957), or
// else the scheme of the repository's configured auth (adoauth.AuthScheme) —
// the rule the daemon applies to a stage's delivered tokens. The repository
// must be configured, so the scheme is never guessed.
func terminalADOCredentialKind(cfg *instance.Config, project apiv1.RepoRef, stated string) (string, error) {
	repo, err := terminalConfiguredRepo(cfg, project)
	if err != nil {
		return "", err
	}
	scheme := stated
	if scheme == "" {
		scheme = adoauth.AuthScheme(repo)
	}
	return adoCredentialKindForScheme(scheme)
}
