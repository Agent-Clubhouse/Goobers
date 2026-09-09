package main

import (
	"context"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func (w *workerSeams) installRemoteRecoveryGuard(manager *worktree.Manager) error {
	if w.recoveryEmitter == nil {
		return nil
	}
	return manager.SetCleanupGuard("recovery", func(ctx context.Context, target worktree.CleanupTarget) error {
		return w.publishWorkerRecovery(ctx, manager.Root, target)
	})
}

func (w *workerSeams) publishWorkerRecovery(ctx context.Context, cleanupRoot string, target worktree.CleanupTarget) error {
	if target.OwnerRunID == "" || target.Gaggle == "" || target.CreatedAt.IsZero() {
		return fmt.Errorf("worker recovery requires durable run and gaggle ownership")
	}
	emitter := w.recoveryEmitter
	token, err := workerRecoveryToken(emitter, target.OwnerRunID)
	if err != nil {
		return err
	}
	client, err := claimsclient.NewHTTP(claimsclient.HTTPConfig{BaseURL: emitter.BaseURL, Token: token, RunID: target.OwnerRunID, Client: emitter.Client})
	if err != nil {
		return err
	}
	claims, err := client.ForRunAll(ctx, target.OwnerRunID)
	if err != nil {
		return err
	}
	if len(claims) != 1 || claims[0].RunID != target.OwnerRunID || claims[0].Gaggle != target.Gaggle ||
		claims[0].ItemID == "" || claims[0].ReleasedAt != nil || !claims[0].ExpiresAt.After(time.Now()) {
		return fmt.Errorf("worker recovery requires exactly one current issue claim")
	}
	ctx, cancel := context.WithDeadline(ctx, claims[0].ExpiresAt)
	defer cancel()
	layout := instance.NewLayout(w.root)
	key, err := workerRecoveryRepository(layout, target.RepositoryDigest)
	if err != nil {
		return err
	}
	root, err := prepareRecoveryInventory(w.root)
	if err != nil {
		return err
	}
	publisher := recovery.HTTPArchivePublisher{BaseURL: emitter.BaseURL, Token: token, RunID: target.OwnerRunID, Client: emitter.Client}
	request := recovery.RetentionRequest{
		Repository: target.Path, RepositoryKey: key, RunID: target.OwnerRunID, BaseRef: recoveryCleanupBaseRef(target),
		IdentityTime: target.CreatedAt, RetainUntil: target.CreatedAt.Add(30 * 24 * time.Hour),
		InventoryRoot: root, CleanupRoots: []string{cleanupRoot}, MaxSnapshots: 128, MaxArchiveBytes: 512 << 20, SkipEmpty: true,
		AcknowledgeArchive: func(ctx context.Context, record recovery.Record, archive string) error {
			return publisher.PublishArchive(ctx, claims[0].ItemID, record, archive)
		},
	}
	publication := recoveryCleanupJournal{directory: layout.SchedulerDir(), scrubber: w.scrubber}
	if err := recovery.RetainAbandonedPreparation(ctx, request, publication); err != nil {
		return err
	}
	_, _, err = recovery.Retain(ctx, request, publication)
	return err
}

func workerRecoveryToken(emitter *livejournal.HTTPEmitter, runID string) (string, error) {
	if emitter.Token != "" {
		return emitter.Token, nil
	}
	if emitter.Minter == nil {
		return "", fmt.Errorf("worker recovery has no run-scoped credential")
	}
	return emitter.Minter.Mint(runID, 5*time.Minute)
}

func workerRecoveryRepository(layout instance.Layout, digest string) (string, error) {
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return "", err
	}
	var selected string
	for _, repo := range cfg.Repos {
		ref := apiv1.RepoRef{Provider: apiv1.Provider(repo.Provider), BaseURL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
		url, err := runner.DefaultRepoCloneURL(ref)
		if err != nil {
			return "", err
		}
		if worktree.RepositoryDigest(url) != digest {
			continue
		}
		key := (providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}).CanonicalKey()
		if selected != "" && selected != key {
			return "", fmt.Errorf("worker recovery repository identity is ambiguous")
		}
		selected = key
	}
	if selected == "" {
		return "", fmt.Errorf("worker recovery repository identity is unavailable")
	}
	return selected, nil
}
