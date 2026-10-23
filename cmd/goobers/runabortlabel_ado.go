package main

import (
	"context"

	"github.com/goobers/goobers/providers"
)

// A terminal PR label is an Azure Repos mutation, never an Azure Boards tag
// update. Keep the materialized capability's auth scheme and secret registrar.
var newADORunAbortLabelProvider = func(repo providers.RepositoryRef, kind string, source providers.TokenSource, registrar providers.SecretRegistrar) (workItemUpdater, error) {
	credential, err := providers.NewADOTokenSourceCredentialSource(kind, source)
	if err != nil {
		return nil, err
	}
	provider := providers.NewADOProvider(repo.Owner, repo.Project, "",
		providers.WithADOCredentialSource(credential), providers.WithADOSecretRegistrar(registrar))
	return adoTerminalPRLabelUpdater{provider}, nil
}

type adoTerminalPRLabelUpdater struct{ *providers.ADOProvider }

func (p adoTerminalPRLabelUpdater) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	if err := p.AddPullRequestLabels(ctx, req.Repository, req.ID, req.AddLabels); err != nil {
		return providers.WorkItem{}, err
	}
	for _, name := range req.RemoveLabels {
		if err := p.RemovePullRequestLabel(ctx, req.Repository, req.ID, name); err != nil {
			return providers.WorkItem{}, err
		}
	}
	return providers.WorkItem{ID: req.ID}, nil
}
