package interactiveaccess

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// Target is a configured source identity. Repository must be empty for backlog
// targets; backlog provider identity is derived exclusively from the gaggle.
type Target struct {
	Kind       string
	Repository apiv1.InteractiveRepositoryIdentity
}

func repositoryIdentity(repo apiv1.RepoRef) apiv1.InteractiveRepositoryIdentity {
	return apiv1.InteractiveRepositoryIdentity{Provider: repo.Provider, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
}

func belongsTo(g *apiv1.Gaggle, id apiv1.InteractiveRepositoryIdentity) bool {
	if repositoryIdentity(g.Spec.Project) == id {
		return true
	}
	for _, repo := range g.Spec.AdditionalRepos {
		if repositoryIdentity(repo) == id {
			return true
		}
	}
	return false
}

func backlogIdentity(g *apiv1.Gaggle) (apiv1.InteractiveRepositoryIdentity, bool) {
	backlog := g.Spec.Backlog
	switch backlog.Provider {
	case apiv1.ProviderGitHub:
		owner, name, ok := strings.Cut(backlog.Project, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return apiv1.InteractiveRepositoryIdentity{}, false
		}
		return apiv1.InteractiveRepositoryIdentity{Provider: backlog.Provider, Owner: owner, Name: name}, true
	case apiv1.ProviderADO:
		// The existing provider topology requires an ADO code project to establish
		// the organization. A GitHub owner must never become an ADO organization.
		if g.Spec.Project.Provider != apiv1.ProviderADO || backlog.Project == "" {
			return apiv1.InteractiveRepositoryIdentity{}, false
		}
		return apiv1.InteractiveRepositoryIdentity{Provider: backlog.Provider, Owner: g.Spec.Project.Owner, Project: backlog.Project}, true
	default:
		return apiv1.InteractiveRepositoryIdentity{}, false
	}
}

func selectSource(g *apiv1.Gaggle, sources map[string]instance.InteractiveCredential, target Target) (instance.InteractiveCredential, error) {
	if g == nil || g.Spec.InteractiveAccess == nil {
		return instance.InteractiveCredential{}, ErrCredentialUnavailable
	}
	policy := g.Spec.InteractiveAccess
	var ref string
	var id apiv1.InteractiveRepositoryIdentity
	switch target.Kind {
	case "backlog":
		if target.Repository != (apiv1.InteractiveRepositoryIdentity{}) {
			return instance.InteractiveCredential{}, ErrCredentialUnavailable
		}
		var ok bool
		id, ok = backlogIdentity(g)
		if !ok {
			return instance.InteractiveCredential{}, ErrCredentialUnavailable
		}
		ref = policy.Credentials.Backlog
	case "repository":
		id = target.Repository
		if !belongsTo(g, id) {
			return instance.InteractiveCredential{}, ErrCredentialUnavailable
		}
		for _, binding := range policy.Credentials.Repositories {
			if binding.Repository == id {
				ref = binding.CredentialRef
				break
			}
		}
	default:
		return instance.InteractiveCredential{}, ErrCredentialUnavailable
	}
	source, ok := sources[ref]
	if !ok || source.Provider != string(id.Provider) || source.Owner != id.Owner || source.Project != id.Project {
		return instance.InteractiveCredential{}, ErrCredentialUnavailable
	}
	if id.Name != "" && source.Repository != id.Name {
		return instance.InteractiveCredential{}, ErrCredentialUnavailable
	}
	return source, nil
}

func actionTarget(action apiv1.InteractiveAction, target Target) bool {
	switch action {
	case "backlog.read", "backlog.edit", "backlog.resolve":
		return target.Kind == "backlog"
	case "repository.read", "pr.repair", "source.proposeChange":
		return target.Kind == "repository"
	default:
		return false
	}
}
