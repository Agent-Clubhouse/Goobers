package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func recoveryCleanupOption(layout instance.Layout, cfg *instance.Config, cleanupRoot string, cloneURL func(apiv1.RepoRef) (string, error), scrubber journal.Scrubber, tel *telemetry.Client) (worktree.ManagerOption, error) {
	identities, err := recoveryRepositoryIdentities(cfg, cloneURL)
	if err != nil {
		return nil, err
	}
	return func(manager *worktree.Manager) {
		callback := recoveryCleanupHandler(layout, cfg, cleanupRoot, identities, scrubber, false, manager, tel)
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

func recoveryCleanupHandler(layout instance.Layout, cfg *instance.Config, cleanupRoot string, identities map[string]string, scrubber journal.Scrubber, terminal bool, manager *worktree.Manager, tel *telemetry.Client) func(context.Context, worktree.CleanupTarget) error {
	return func(ctx context.Context, target worktree.CleanupTarget) error {
		if tel != nil {
			ctx = recovery.WithSnapshotObserver(ctx, tel)
		}
		key, ok := identities[target.RepositoryDigest]
		if !ok || target.OwnerRunID == "" {
			return fmt.Errorf("recovery cleanup requires verified repository and run ownership")
		}
		if _, err := recovery.RefForRun(target.OwnerRunID); err != nil {
			return err
		}
		if strings.TrimSpace(target.BaseRef) == "" {
			return recoveryCleanupHistoricalTarget(ctx, layout, cfg, cleanupRoot, scrubber, manager, key, target)
		}
		return recoveryCleanupCurrentTarget(ctx, layout, cfg, cleanupRoot, scrubber, terminal, manager, key, target)
	}
}

func recoveryCleanupCurrentTarget(ctx context.Context, layout instance.Layout, cfg *instance.Config, cleanupRoot string, scrubber journal.Scrubber, terminal bool, manager *worktree.Manager, key string, target worktree.CleanupTarget) error {
	reader, identity, err := recoveryCleanupRun(layout, target)
	if err != nil {
		return err
	}
	captureAt, err := recoveryCaptureTime(ctx, reader, identity.StartedAt, terminal)
	if err != nil {
		return err
	}
	request, err := recoveryCleanupRequest(layout, cfg, cleanupRoot, manager, key, target, captureAt)
	if err != nil {
		return err
	}
	baseRef, err := recoveryCleanupBaseRef(target)
	if err != nil {
		return err
	}
	request.BaseRef = baseRef
	publication := recoveryCleanupJournal{directory: layout.SchedulerDir(), scrubber: scrubber}
	if err := recovery.RetainAbandonedPreparation(ctx, request, publication); err != nil {
		return err
	}
	_, _, err = recovery.Retain(ctx, request, publication)
	return err
}

func recoveryCleanupHistoricalTarget(ctx context.Context, layout instance.Layout, cfg *instance.Config, cleanupRoot string, scrubber journal.Scrubber, manager *worktree.Manager, key string, target worktree.CleanupTarget) error {
	reader, identity, err := recoveryCleanupRun(layout, target)
	if err != nil {
		return retainUnknownBase(err)
	}
	events, err := reader.Events()
	if err != nil {
		return retainUnknownBase(fmt.Errorf("terminal run evidence unavailable: %w", err))
	}
	if journal.PhaseFromEvents(events) == journal.PhaseRunning {
		return retainUnknownBase(fmt.Errorf("owning run is not terminal"))
	}
	captureAt, err := recoveryWindowTime(events, identity.StartedAt)
	if err != nil {
		return retainUnknownBase(fmt.Errorf("terminal run evidence unavailable: %w", err))
	}
	request, err := recoveryCleanupRequest(layout, cfg, cleanupRoot, manager, key, target, captureAt)
	if err != nil {
		return retainUnknownBase(err)
	}
	publication := recoveryCleanupJournal{directory: layout.SchedulerDir(), scrubber: scrubber}
	if err := recovery.RetainAbandonedPreparation(ctx, request, publication); err != nil {
		return retainUnknownBase(fmt.Errorf("abandoned recovery preparation could not be retained: %w", err))
	}
	if err := worktree.VerifyCleanupTargetUnchanged(ctx, target); err != nil {
		return retainUnknownBase(err)
	}
	return nil
}

func recoveryCleanupRun(layout instance.Layout, target worktree.CleanupTarget) (*journal.Reader, journal.RunIdentity, error) {
	reader, err := journal.OpenReadOnly(filepath.Join(layout.RunsDir(), target.OwnerRunID))
	if err != nil {
		return nil, journal.RunIdentity{}, fmt.Errorf("owning run journal unavailable: %w", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return nil, journal.RunIdentity{}, fmt.Errorf("owning run identity unavailable: %w", err)
	}
	if identity.RunID != target.OwnerRunID || identity.StartedAt.IsZero() {
		return nil, journal.RunIdentity{}, fmt.Errorf("recovery run identity does not match cleanup ownership")
	}
	return reader, identity, nil
}

func recoveryCleanupRequest(layout instance.Layout, cfg *instance.Config, cleanupRoot string, manager *worktree.Manager, key string, target worktree.CleanupTarget, captureAt time.Time) (recovery.RetentionRequest, error) {
	root, err := prepareRecoveryInventory(layout.Root)
	if err != nil {
		return recovery.RetentionRequest{}, fmt.Errorf("recovery inventory unavailable: %w", err)
	}
	recoveryCfg := cfg.Retention.RecoveryEffective()
	retainWindow, err := recoveryCfg.RetainWindowEffective()
	if err != nil {
		return recovery.RetentionRequest{}, fmt.Errorf("recovery retention policy unavailable: %w", err)
	}
	return recovery.RetentionRequest{
		Repository: target.Path, RepositoryKey: key, RunID: target.OwnerRunID,
		IdentityTime: captureAt, RetainUntil: captureAt.Add(retainWindow),
		InventoryRoot: root, CleanupRoots: []string{cleanupRoot},
		MaxSnapshots: recoveryCfg.MaxSnapshotsEffective(), MaxArchiveBytes: recoveryCfg.MaxArchiveBytesEffective(), SkipEmpty: true,
		EvictFull: recoveryEvictFunc(layout, cfg, manager, key),
	}, nil
}

func retainUnknownBase(err error) error {
	return worktree.RetainCleanupTarget(worktree.CleanupDispositionUnknownBase, err)
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
		callback := recoveryCleanupHandler(layout, cfg, manager.Root, identities, journal.NewRegistryScrubber(), true, manager, nil)
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
