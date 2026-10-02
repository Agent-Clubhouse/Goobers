package main

import (
	"context"

	"github.com/goobers/goobers/internal/branchretention"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/instanceannotations"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func retentionService(l instance.Layout) branchretention.Service {
	return branchretention.Service{Root: l.Root, UnknownRepository: ErrItemRepositoryUnknown, KindAuthorizesCustody: recoveryItemKindAuthorizesCustody,
		Repositories: func(runID string) (map[string]instanceannotations.ItemRepository, error) {
			return instanceannotations.ForInstance(l.SchedulerDir()).AllItemRepositories(l.SchedulerDir(), runID)
		},
		ItemParked: func(ctx context.Context, root, id string, entry instanceannotations.ItemRepository) (bool, error) {
			return retentionItemParked(ctx, root, id, recordedItemRepo{repo: entry.Repository, kind: entry.Kind})
		},
	}
}

var retentionItemParked = func(ctx context.Context, root, id string, entry recordedItemRepo) (bool, error) {
	reader := branchretention.ItemReader{UnknownRepository: ErrItemRepositoryUnknown, NewProvider: func(root string, repo providers.RepositoryRef) (providers.Provider, error) {
		return newProviderForStage(root, repo, true, withStageProviderConfiguredADOAuth())
	}}
	return reader.Parked(ctx, root, id, instanceannotations.ItemRepository{Repository: entry.repo, Kind: entry.kind})
}

func configureBranchRetention(ctx context.Context, l instance.Layout, runsByRoot map[string]string, branchReferences map[string]map[string][]string, opts *worktree.RetentionOptions) {
	retentionService(l).Configure(ctx, runsByRoot, branchReferences, opts)
}
