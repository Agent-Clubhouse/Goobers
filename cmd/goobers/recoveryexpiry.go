package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func retireExpiredRecovery(ctx context.Context, layout instance.Layout, setup *schedulerSetup, managers []*worktree.Manager, runsByRoot map[string]string, stdout, stderr io.Writer) error {
	root := filepath.Join(layout.Root, "recovery")
	entries, err := recovery.ReadInventory(ctx, root, 128)
	if err != nil {
		return err
	}
	var failures error
	for _, entry := range entries {
		err := retireExpiredRecoveryEntry(ctx, root, setup, managers, runsByRoot, entry, stdout)
		if err != nil {
			pf(stderr, "warning: recovery retention failed run=%q ref=%q: %v\n", entry.Record.RunID, entry.Record.Ref, err)
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func retireExpiredRecoveryEntry(ctx context.Context, root string, setup *schedulerSetup, managers []*worktree.Manager, runsByRoot map[string]string, entry recovery.InventoryEntry, stdout io.Writer) error {
	manager, runDir, err := recoveryRetentionOwner(entry.Record.RunID, managers, runsByRoot)
	if err != nil {
		return err
	}
	_, err = journal.WithIdleRunReader(ctx, runDir, func(reader *journal.Reader) error {
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			return err
		}
		eligible, err := recoveryDeadlineExpired(reader, record, time.Now().UTC())
		if err != nil || !eligible {
			return err
		}
		url, err := recoveryRetentionCloneURL(setup.Config, record.RepositoryKey)
		if err != nil {
			return err
		}
		found, err := manager.WithExistingMirror(ctx, url, func(repository string) error {
			if setup.Config.Retention.DryRun {
				pf(stdout, "retention candidate kind=recovery rule=retention-window run=%q ref=%q\n", record.RunID, record.Ref)
				return nil
			}
			_, err := recovery.RetireSnapshot(ctx, root, record, func(current recovery.Record) error {
				return recovery.DeleteSnapshotRef(ctx, repository, current)
			})
			return err
		})
		if err == nil && !found {
			return fmt.Errorf("recovery retention requires an existing managed mirror")
		}
		return err
	})
	return err
}

func recoveryRetentionOwner(runID string, managers []*worktree.Manager, runsByRoot map[string]string) (*worktree.Manager, string, error) {
	var owner *worktree.Manager
	var runDir string
	for _, manager := range managers {
		candidate := filepath.Join(runsByRoot[manager.Root], runID)
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if !info.IsDir() || owner != nil {
			return nil, "", fmt.Errorf("recovery retention requires unambiguous real run directory")
		}
		owner, runDir = manager, candidate
	}
	if owner == nil {
		return nil, "", fmt.Errorf("recovery retention requires owning run journal")
	}
	return owner, runDir, nil
}

func recoveryDeadlineExpired(reader *journal.Reader, record recovery.Record, now time.Time) (bool, error) {
	identity, err := reader.Identity()
	if err != nil {
		return false, err
	}
	if identity.RunID != record.RunID || identity.StartedAt.IsZero() {
		return false, fmt.Errorf("recovery retention run identity mismatch")
	}
	events, err := reader.Events()
	if err != nil {
		return false, err
	}
	if !terminalRunPhase(journal.PhaseFromEvents(events)) {
		return false, nil
	}
	finished, err := recoveryWindowTime(events, identity.StartedAt)
	if err != nil {
		return false, err
	}
	// Stage capture may be older than terminal renewal. Never prune in the
	// terminal window just because renewal has not yet acknowledged its sidecar.
	return !now.Before(record.RetainUntil) && !now.Before(finished.Add(30*24*time.Hour)), nil
}

func recoveryRetentionCloneURL(cfg *instance.Config, key string) (string, error) {
	clone := repoCloneURL
	if clone == nil {
		clone = runner.DefaultRepoCloneURL
	}
	var url string
	for _, repo := range cfg.Repos {
		identity := providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
		if identity.CanonicalKey() != key {
			continue
		}
		if url != "" {
			return "", fmt.Errorf("ambiguous recovery repository configuration")
		}
		var err error
		url, err = clone(apiv1.RepoRef{Provider: apiv1.Provider(repo.Provider), BaseURL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name})
		if err != nil {
			return "", err
		}
	}
	if url == "" {
		return "", fmt.Errorf("recovery repository is not configured")
	}
	return url, nil
}
