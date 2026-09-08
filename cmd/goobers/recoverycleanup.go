package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func recoveryCleanupOption(layout instance.Layout, cfg *instance.Config, cleanupRoot string, cloneURL func(apiv1.RepoRef) (string, error), scrubber journal.Scrubber) (worktree.ManagerOption, error) {
	identities := make(map[string]string)
	for _, repo := range cfg.Repos {
		project := apiv1.RepoRef{Provider: apiv1.Provider(repo.Provider), BaseURL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
		url, err := cloneURL(project)
		if err != nil {
			return nil, err
		}
		key := (providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}).CanonicalKey()
		digest := worktree.RepositoryDigest(url)
		if existing, ok := identities[digest]; ok && existing != key {
			return nil, fmt.Errorf("recovery clone URL maps to multiple repository identities")
		}
		identities[digest] = key
	}
	callback := func(ctx context.Context, target worktree.CleanupTarget) error {
		key, ok := identities[target.RepositoryDigest]
		if !ok || target.OwnerRunID == "" {
			return fmt.Errorf("recovery cleanup requires verified repository and run ownership")
		}
		if _, err := recovery.RefForRun(target.OwnerRunID); err != nil {
			return err
		}
		reader, err := journal.OpenReadOnly(filepath.Join(layout.RunsDir(), target.OwnerRunID))
		if err != nil {
			return err
		}
		identity, err := reader.Identity()
		if err != nil {
			return err
		}
		if identity.RunID != target.OwnerRunID || identity.StartedAt.IsZero() {
			return fmt.Errorf("recovery run identity does not match cleanup ownership")
		}
		root, err := prepareRecoveryInventory(layout.Root)
		if err != nil {
			return err
		}
		_, _, err = recovery.Retain(ctx, recovery.RetentionRequest{
			Repository: target.Path, RepositoryKey: key, RunID: target.OwnerRunID,
			BaseRef: "main", IdentityTime: identity.StartedAt, RetainUntil: identity.StartedAt.Add(30 * 24 * time.Hour),
			InventoryRoot: root, CleanupRoots: []string{cleanupRoot}, MaxSnapshots: 128, MaxArchiveBytes: 512 << 20, SkipEmpty: true,
		}, recoveryCleanupJournal{directory: layout.SchedulerDir(), scrubber: scrubber})
		return err
	}
	return func(manager *worktree.Manager) {
		// Both arguments are fixed, valid values; installing the same named
		// guard replaces it on reload rather than stacking archive callbacks.
		_ = manager.SetCleanupGuard("recovery", callback)
	}, nil
}

func prepareRecoveryInventory(instanceRoot string) (string, error) {
	root := filepath.Join(instanceRoot, "recovery")
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("recovery inventory must be a real directory")
	}
	if err := durability.SyncDir(instanceRoot); err != nil {
		return "", err
	}
	return root, nil
}

type recoveryCleanupJournal struct {
	directory string
	scrubber  journal.Scrubber
}

func (l recoveryCleanupJournal) Append(event journal.Event) error {
	log, _, err := journal.OpenInstanceLog(l.directory, journal.WithScrubber(l.scrubber))
	if err != nil {
		return err
	}
	return errors.Join(log.Append(event), log.Close())
}
