package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func recoveryCleanupOption(layout instance.Layout, cfg *instance.Config, cleanupRoot string, cloneURL func(apiv1.RepoRef) (string, error), scrubber journal.Scrubber) (worktree.ManagerOption, error) {
	identities, err := recoveryRepositoryIdentities(cfg, cloneURL)
	if err != nil {
		return nil, err
	}
	return func(manager *worktree.Manager) {
		callback := recoveryCleanupHandler(layout, cfg, cleanupRoot, identities, scrubber, false, manager)
		_ = manager.SetCleanupGuard("recovery", callback)
	}, nil
}

// recoveryRepositoryIdentities maps each configured repository's clone-URL
// digest to its canonical identity, validated once so an ambiguous mapping
// fails at wiring time rather than inside a live cleanup guard.
func recoveryRepositoryIdentities(cfg *instance.Config, cloneURL func(apiv1.RepoRef) (string, error)) (map[string]string, error) {
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
	return identities, nil
}

func recoveryCleanupHandler(layout instance.Layout, cfg *instance.Config, cleanupRoot string, identities map[string]string, scrubber journal.Scrubber, terminal bool, manager *worktree.Manager) func(context.Context, worktree.CleanupTarget) error {
	return func(ctx context.Context, target worktree.CleanupTarget) error {
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
		captureAt, err := recoveryCaptureTime(ctx, reader, identity.StartedAt, terminal)
		if err != nil {
			return err
		}
		root, err := prepareRecoveryInventory(layout.Root)
		if err != nil {
			return err
		}
		recoveryCfg := cfg.Retention.RecoveryEffective()
		retainWindow, err := recoveryCfg.RetainWindowEffective()
		if err != nil {
			return err
		}
		baseRef, err := recoveryCleanupBaseRef(target)
		if err != nil {
			return err
		}
		request := recovery.RetentionRequest{
			Repository: target.Path, RepositoryKey: key, RunID: target.OwnerRunID,
			BaseRef: baseRef, IdentityTime: captureAt, RetainUntil: captureAt.Add(retainWindow),
			InventoryRoot: root, CleanupRoots: []string{cleanupRoot},
			MaxSnapshots: recoveryCfg.MaxSnapshotsEffective(), MaxArchiveBytes: recoveryCfg.MaxArchiveBytesEffective(), SkipEmpty: true,
			EvictFull: recoveryEvictFunc(layout, cfg, manager, key),
		}
		publication := recoveryCleanupJournal{directory: layout.SchedulerDir(), scrubber: scrubber}
		if err := recovery.RetainAbandonedPreparation(ctx, request, publication); err != nil {
			return err
		}
		_, _, err = recovery.Retain(ctx, request, publication)
		return err
	}
}

func recoveryCleanupBaseRef(target worktree.CleanupTarget) (string, error) {
	baseRef := strings.TrimSpace(target.BaseRef)
	if baseRef == "" {
		return "", fmt.Errorf("recovery cleanup requires the owning run's base reference")
	}
	return baseRef, nil
}

// Standalone abort/startup/stall finalizers may construct their own Manager.
// Resolve configuration only when an actual owned worktree needs cleanup, so
// already-clean runs can still release claims even with unavailable config.
func installTerminalRecoveryGuard(layout instance.Layout, manager *worktree.Manager) error {
	return manager.SetCleanupGuard("recovery", func(ctx context.Context, target worktree.CleanupTarget) error {
		cfg, err := instance.LoadConfig(layout.ConfigFile())
		if err != nil {
			return fmt.Errorf("load recovery configuration before cleanup: %w", err)
		}
		cloneURL := repoCloneURL
		if cloneURL == nil {
			cloneURL = runner.DefaultRepoCloneURL
		}
		identities, err := recoveryRepositoryIdentities(cfg, cloneURL)
		if err != nil {
			return err
		}
		callback := recoveryCleanupHandler(layout, cfg, manager.Root, identities, journal.NewRegistryScrubber(), true, manager)
		return callback(ctx, target)
	})
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
