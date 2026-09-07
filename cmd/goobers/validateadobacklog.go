package main

import (
	"context"
	"fmt"
	"io"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

var targetADOBacklogReachable = adoBacklogReachable

func checkConfiguredRepositoryAccess(root, configDir, configFile string, cfg *instance.Config, set *instance.ConfigSet, stores credentials.StoreResolver, stdout io.Writer, diagnostics *diagnosticCollector) bool {
	if !checkTargetRepositoriesAtFile(cfg.Repos, stores, stdout, diagnosticFile(root, configFile), diagnostics) {
		return false
	}
	return checkADOBacklogProjects(root, configDir, cfg, set, stores, stdout, diagnostics)
}

func adoBacklogReachable(ctx context.Context, repo instance.RepoRef, project string, stores credentials.StoreResolver) error {
	registry := journal.NewRegistryScrubber()
	provider, err := adoauth.Provider(repo, nil, registry, nil, nil, stores)
	if err == nil {
		var page providers.ListWorkItemsPageInfo
		_, err = provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
			Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Owner: repo.Owner, Project: project, Name: repo.Name},
			State:      "all", Limit: 1, PageInfo: &page,
		})
	}
	if err != nil {
		scrubber := journal.Chain(registry, journal.NewPatternScrubber())
		return fmt.Errorf("azure Boards query failed: %s", scrubber.Scrub([]byte(err.Error())))
	}
	return nil
}

// Repository Git access does not prove Boards project access. In particular,
// spec.backlog.project may legitimately name another project in the same org.
func checkADOBacklogProjects(root, configDir string, cfg *instance.Config, set *instance.ConfigSet, stores credentials.StoreResolver, stdout io.Writer, diagnostics *diagnosticCollector) bool {
	ok := true
	for _, gaggle := range set.Gaggles {
		if gaggle.Spec.Backlog.Provider != "ado" {
			continue
		}
		project := gaggle.Spec.Backlog.Project
		repo, found := configuredRepoForProject(cfg, gaggle.Spec.Project)
		var err error
		switch {
		case project == "":
			err = fmt.Errorf("spec.backlog.project is required for Azure Boards")
		case !found || repo.Provider != "ado":
			err = fmt.Errorf("no configured ADO repository credential for this gaggle")
		default:
			ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
			err = targetADOBacklogReachable(ctx, repo, project, stores)
			cancel()
		}
		if err == nil {
			pf(stdout, "BACKLOG Gaggle/%s: Azure Boards project %q is reachable\n", gaggle.Name, project)
			continue
		}
		message := fmt.Sprintf("Azure Boards project %q: %s; check spec.backlog.project spelling and Work Items read access in organization %q (the Boards and repository projects may differ)", project, scrubRepositoryError(err, ""), repo.Owner)
		pf(stdout, "ERROR BACKLOG001 Gaggle/%s: %s\n", gaggle.Name, message)
		diagnostics.add(gaggleDiagnosticFile(root, configDir, set, gaggle.Name), "/spec/backlog/project", "BACKLOG001", string(validate.Error), message)
		ok = false
	}
	return ok
}
