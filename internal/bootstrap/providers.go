package bootstrap

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// BacklogProviderFor constructs a backlog provider for a gaggle's BacklogRef,
// authenticated with token, and returns the RepositoryRef the scheduler polls.
// Credentials are passed in (resolved from a Key Vault secret ref by the caller),
// never read from config. Credentialed ADO providers require registrar so every
// form used on the wire is scrubbed at output boundaries.
func BacklogProviderFor(backlog apiv1.BacklogRef, token string, registrar providers.SecretRegistrar) (providers.BacklogProvider, providers.RepositoryRef, error) {
	switch backlog.Provider {
	case apiv1.ProviderGitHub:
		owner, name, ok := splitProject(backlog.Project)
		if !ok {
			return nil, providers.RepositoryRef{}, fmt.Errorf("github backlog project %q must be owner/name", backlog.Project)
		}
		repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: owner, Name: name}
		return providers.NewGitHubProvider(token), repo, nil
	case apiv1.ProviderADO:
		org, project, ok := splitProject(backlog.Project)
		if !ok {
			return nil, providers.RepositoryRef{}, fmt.Errorf("ado backlog project %q must be organization/project", backlog.Project)
		}
		if token != "" && registrar == nil {
			return nil, providers.RepositoryRef{}, fmt.Errorf("ado backlog provider requires a secret registrar")
		}
		repo := providers.RepositoryRef{Provider: providers.ProviderADO, Project: project}
		return providers.NewADOProvider(org, project, token, providers.WithADOSecretRegistrar(registrar)), repo, nil
	default:
		return nil, providers.RepositoryRef{}, fmt.Errorf("unsupported backlog provider %q", backlog.Provider)
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
