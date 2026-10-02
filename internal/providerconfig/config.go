// Package providerconfig resolves provider repository roles and backlog settings.
package providerconfig

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// ApplyBacklogProject resolves the ref the named gaggle's backlog role
// addresses, given the routed code repository.
//
// For an ADO backlog on an ADO project it overrides only the project tier of
// routed with the backlog project. Organization, name, and credentials stay
// the routed code repo's (the ADO provider is org-scoped and the backlog lives
// under the same organization and auth).
//
// For a GitHub or Gitea backlog on an ADO project (topology (b),
// docs/design/ado-parity-dsl-2-0.md §7.2) it returns the backlog provider's
// own ref (BacklogProviderRef), so backlog work opens that provider.
//
// It returns routed unchanged when the gaggle is absent, the backlog project
// is undeclared, or the topology is not one this release routes by role.
func ApplyBacklogProject(set *instance.ConfigSet, gaggle string, routed providers.RepositoryRef) providers.RepositoryRef {
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if g.Name != gaggle {
			continue
		}
		backlog := g.Spec.Backlog
		if backlog.Project == "" {
			return routed
		}
		if CrossProviderBacklog(apiv1.Provider(routed.Provider), backlog.Provider) {
			ref, err := BacklogProviderRef(g.Name, g.Spec.Project, backlog)
			if err != nil {
				return routed
			}
			return ref
		}
		if backlog.Provider != apiv1.ProviderADO {
			return routed
		}
		ref := routed
		ref.Project = backlog.Project
		return ref
	}
	return routed
}

// CrossProviderBacklog reports whether a gaggle whose code lives on project
// keeps its backlog on backlog, a different provider, in a topology that is
// routed by role: a GitHub or Gitea backlog for Azure DevOps code (topology
// (b), docs/design/ado-parity-dsl-2-0.md §7.2). Issue work then opens the
// backlog provider and pull-request work the project provider. Every other
// combination keeps the single routed provider it always used.
func CrossProviderBacklog(project, backlog apiv1.Provider) bool {
	if project != apiv1.ProviderADO {
		return false
	}
	return backlog == apiv1.ProviderGitHub || backlog == apiv1.ProviderGitea
}

// BacklogProviderRef builds the RepositoryRef of a gaggle's backlog provider
// from its spec: owner/name from backlog.project on GitHub and Gitea, the
// backlog project on Azure DevOps, and backlog.baseUrl as the service URL.
func BacklogProviderRef(gaggle string, project apiv1.RepoRef, backlog apiv1.BacklogRef) (providers.RepositoryRef, error) {
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
			return providers.RepositoryRef{}, fmt.Errorf(
				"gaggle %q backlog project %q must be owner/name",
				gaggle,
				backlog.Project,
			)
		}
		// The project tier is Azure DevOps addressing; a GitHub or Gitea
		// repository has none.
		repo.Owner, repo.Project, repo.Name = owner, "", name
	case providers.ProviderADO:
		repo.Project = backlog.Project
	}
	return repo, nil
}

// BacklogProviderRepo is the repository a stage opens its backlog provider
// for. A backlog on the routed provider (GitHub, or the ADO project split)
// keeps opening routed exactly as before and only addresses backlog in its
// work-item calls; a backlog on another provider (topology (b)) opens that
// provider.
func BacklogProviderRepo(routed, backlog providers.RepositoryRef) providers.RepositoryRef {
	if BacklogOnOtherProvider(routed, backlog) {
		return backlog
	}
	return routed
}

// BacklogOnOtherProvider reports whether backlog work leaves the routed
// provider: topology (b), a GitHub or Gitea backlog for Azure DevOps code
// (CrossProviderBacklog).
func BacklogOnOtherProvider(routed, backlog providers.RepositoryRef) bool {
	return CrossProviderBacklog(apiv1.Provider(routed.Provider), apiv1.Provider(backlog.Provider))
}

// GaggleADODoneStates converts the named gaggle's backlog.doneStates into the
// provider form. It reports false when the gaggle is absent, its backlog is
// not ADO, or it declares no doneStates.
func GaggleADODoneStates(set *instance.ConfigSet, gaggle string) (providers.ADODoneStates, bool) {
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if g.Name != gaggle {
			continue
		}
		backlog := g.Spec.Backlog
		if backlog.Provider != apiv1.ProviderADO || backlog.DoneStates == nil {
			return providers.ADODoneStates{}, false
		}
		states := providers.ADODoneStates{ByType: backlog.DoneStates.ByType}
		for _, category := range backlog.DoneStates.Categories {
			states.Categories = append(states.Categories, string(category))
		}
		return states, true
	}
	return providers.ADODoneStates{}, false
}
