package main

import (
	"context"
	"fmt"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

var newGuidedADOClient = func(repo instance.RepoRef, stores credentials.StoreResolver, registrar providers.SecretRegistrar) (connectADOSeeder, error) {
	return adoauth.Provider(repo, nil, registrar, nil, nil, stores)
}

func prepareGuidedADORepository(ctx context.Context, root string, gaggle apiv1.Gaggle, input guidedPrepareRepositoryRequest, response *guidedRepositoryReadiness) (err error) {
	registry := journal.NewRegistryScrubber()
	defer func() {
		if err != nil {
			err = fmt.Errorf("prepare Azure Boards: %s", journal.Chain(registry, journal.NewPatternScrubber()).Scrub([]byte(err.Error())))
		}
	}()
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		return err
	}
	repo, found := configuredRepoForProject(cfg, gaggle.Spec.Project)
	if !found || repo.Provider != "ado" || gaggle.Spec.Backlog.Provider != apiv1.ProviderADO || gaggle.Spec.Backlog.Project == "" {
		return fmt.Errorf("a configured ADO repository and Azure Boards backlog project are required")
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return err
	}
	client, err := newGuidedADOClient(repo, stores, registry)
	if err != nil {
		return err
	}
	target := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: repo.Owner, Project: gaggle.Spec.Backlog.Project, Name: repo.Name}
	count, complete, err := adoSelectorCount(ctx, client, target, response.SelectorLabels)
	if err != nil {
		return err
	}
	response.TagMatchCount = &count
	response.TagScanComplete = complete
	if !input.Apply || !input.CreateStarterIssue || count != 0 {
		return nil
	}
	if !complete {
		return fmt.Errorf("candidate scan is incomplete; cannot conclude that a starter task is needed")
	}
	opts := connectOptions{ado: &connectADORepo{Organization: repo.Owner, Project: repo.Project, Repository: repo.Name}}
	catalog := connectADOSeedCatalog(opts, response.SelectorLabels)
	var result onboardingActionResult
	if err := seedADOStarter(ctx, client, target, catalog, &result); err != nil {
		return err
	}
	response.StarterIssueCreated = slices.Contains(result.Created, "issue:"+connectSeedIssueID)
	count, complete, err = adoSelectorCount(ctx, client, target, response.SelectorLabels)
	response.TagScanComplete = complete
	return err
}
