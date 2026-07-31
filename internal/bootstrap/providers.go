package bootstrap

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// BacklogProviderFor constructs a backlog provider while allowing ADO to use an
// Entra credential source instead of a fixed PAT. Credentials are passed in,
// never read from config. Credentialed ADO providers require registrar so every
// form used on the wire is scrubbed at output boundaries.
func BacklogProviderFor(backlog apiv1.BacklogRef, token string, adoSource providers.ADOCredentialSource, registrar providers.SecretRegistrar, rateObserver providers.RateLimitObserver) (providers.BacklogProvider, providers.RepositoryRef, error) {
	switch backlog.Provider {
	case apiv1.ProviderGitHub:
		owner, name, ok := splitProject(backlog.Project)
		if !ok {
			return nil, providers.RepositoryRef{}, fmt.Errorf("github backlog project %q must be owner/name", backlog.Project)
		}
		repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: owner, Name: name}
		return providers.NewGitHubProvider(token, providers.WithRateLimitObserver(rateObserver)), repo, nil
	case apiv1.ProviderGitea:
		owner, name, ok := splitProject(backlog.Project)
		if !ok {
			return nil, providers.RepositoryRef{}, fmt.Errorf("gitea backlog project %q must be owner/name", backlog.Project)
		}
		if backlog.BaseURL == "" {
			return nil, providers.RepositoryRef{}, fmt.Errorf("gitea backlog %q requires baseUrl (self-hosted Gitea has no fixed host)", backlog.Project)
		}
		repo := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: owner, Name: name, URL: backlog.BaseURL}
		return providers.NewGiteaProvider(backlog.BaseURL, token, providers.WithGiteaRateLimitObserver(rateObserver)), repo, nil
	case apiv1.ProviderADO:
		org, project, ok := splitProject(backlog.Project)
		if !ok {
			return nil, providers.RepositoryRef{}, fmt.Errorf("ado backlog project %q must be organization/project", backlog.Project)
		}
		if (token != "" || adoSource != nil) && registrar == nil {
			return nil, providers.RepositoryRef{}, fmt.Errorf("ado backlog provider requires a secret registrar")
		}
		repo := providers.RepositoryRef{Provider: providers.ProviderADO, Project: project}
		options := []func(*providers.ADOProvider){
			providers.WithADOSecretRegistrar(registrar),
			providers.WithADORateLimitObserver(rateObserver),
		}
		if adoSource != nil {
			token = ""
			options = append(options, providers.WithADOCredentialSource(adoSource))
		}
		return providers.NewADOProvider(org, project, token, options...), repo, nil
	default:
		return nil, providers.RepositoryRef{}, fmt.Errorf("unsupported backlog provider %q", backlog.Provider)
	}
}

// RepoProviderFor is the repo/PR-side kind-dispatch constructor, the
// provider-neutral composition counterpart to BacklogProviderFor. It mirrors
// the ADO precedent exactly and adds the gitea branch: switch on repo.Provider
// and return a providers.RepoProvider plus the addressing providers.RepositoryRef.
// Credentials are passed in, never read from config. It is additive — the
// existing hardcoded newGitHubProvider()/providers.NewGitHubProvider() call
// sites are unaffected; only genuine kind-dispatch paths route through here.
func RepoProviderFor(repo apiv1.RepoRef, token string, adoSource providers.ADOCredentialSource, registrar providers.SecretRegistrar, rateObserver providers.RateLimitObserver) (providers.RepoProvider, providers.RepositoryRef, error) {
	switch repo.Provider {
	case apiv1.ProviderGitHub:
		ref := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}
		return providers.NewGitHubProvider(token, providers.WithRateLimitObserver(rateObserver)), ref, nil
	case apiv1.ProviderGitea:
		if repo.BaseURL == "" {
			return nil, providers.RepositoryRef{}, fmt.Errorf("gitea repo %s/%s requires baseUrl (self-hosted Gitea has no fixed host)", repo.Owner, repo.Name)
		}
		ref := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: repo.Owner, Name: repo.Name, URL: repo.BaseURL}
		return providers.NewGiteaProvider(repo.BaseURL, token, providers.WithGiteaRateLimitObserver(rateObserver)), ref, nil
	case apiv1.ProviderADO:
		if (token != "" || adoSource != nil) && registrar == nil {
			return nil, providers.RepositoryRef{}, fmt.Errorf("ado repo provider requires a secret registrar")
		}
		ref := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
		options := []func(*providers.ADOProvider){
			providers.WithADOSecretRegistrar(registrar),
			providers.WithADORateLimitObserver(rateObserver),
		}
		if adoSource != nil {
			token = ""
			options = append(options, providers.WithADOCredentialSource(adoSource))
		}
		return providers.NewADOProvider(repo.Owner, repo.Project, token, options...), ref, nil
	default:
		return nil, providers.RepositoryRef{}, fmt.Errorf("unsupported repo provider %q", repo.Provider)
	}
}

func splitProject(project string) (string, string, bool) {
	owner, name, ok := strings.Cut(project, "/")
	if !ok || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

// BacklogWorkflows returns the names of workflows in a gaggle that declare a
// backlog-item trigger — i.e. the workflows the scheduler should feed from the
// gaggle's backlog.
func (l *Loaded) BacklogWorkflows(gaggleName string) []string {
	var names []string
	for _, w := range l.Workflows {
		if w.Spec.Gaggle != gaggleName {
			continue
		}
		for _, tr := range w.Spec.Triggers {
			if tr.Type == apiv1.TriggerBacklogItem {
				names = append(names, w.Name)
				break
			}
		}
	}
	return names
}
