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

// dryRun is the retention pass's resolved decision (operator retention.dryRun
// or an unelapsed first-enable grace window, #4253). Retiring a snapshot
// deletes its refs, so it has to observe the same window the worktree sweep
// does — otherwise a grace period that only reports worktrees would still be
// destroying recovery snapshots underneath it.
func retireExpiredRecovery(ctx context.Context, layout instance.Layout, setup *schedulerSetup, managers []*worktree.Manager, runsByRoot map[string]string, dryRun bool, stdout, stderr io.Writer) error {
	root := filepath.Join(layout.Root, "recovery")
	entries, err := recovery.ReadInventory(ctx, root, 128)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	operatorEvents, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		return err
	}
	entries, err = prioritizeAbandonedRecovery(entries, operatorEvents)
	if err != nil {
		return err
	}
	var failures error
	for _, entry := range entries {
		err := retireExpiredRecoveryEntry(ctx, root, setup, managers, runsByRoot, entry, operatorEvents, dryRun, stdout)
		if err != nil {
			pf(stderr, "warning: recovery retention failed run=%q ref=%q: %v\n", entry.Record.RunID, entry.Record.Ref, err)
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func prioritizeAbandonedRecovery(entries []recovery.InventoryEntry, operatorEvents []journal.Event) ([]recovery.InventoryEntry, error) {
	prioritized := make([]recovery.InventoryEntry, 0, len(entries))
	remaining := make([]recovery.InventoryEntry, 0, len(entries))
	for _, entry := range entries {
		current, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			return nil, err
		}
		abandoned, err := recovery.ExplicitlyAbandoned(operatorEvents, current)
		if err != nil {
			return nil, err
		}
		if abandoned {
			prioritized = append(prioritized, entry)
		} else {
			remaining = append(remaining, entry)
		}
	}
	return append(prioritized, remaining...), nil
}

func retireExpiredRecoveryEntry(ctx context.Context, root string, setup *schedulerSetup, managers []*worktree.Manager, runsByRoot map[string]string, entry recovery.InventoryEntry, operatorEvents []journal.Event, dryRun bool, stdout io.Writer) error {
	manager, runDir, err := recoveryRetentionOwner(entry.Record.RunID, managers, runsByRoot)
	if err != nil {
		return err
	}
	_, err = journal.WithIdleRunReader(ctx, runDir, func(reader *journal.Reader) error {
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			return err
		}
		eligible, err := recoveryRetirementEligible(reader, record, time.Now().UTC(), operatorEvents)
		if err != nil {
			return err
		}
		var landed []recovery.LandedHead
		if !eligible {
			phase, err := reader.PhaseBounded(ctx)
			if err != nil || !terminalRunPhase(phase) {
				return err
			}
			project, err := recoveryConfiguredProject(setup.Config, record.RepositoryKey)
			if err != nil {
				return err
			}
			route, err := recoveryLandingRoute(project)
			if err != nil {
				return err
			}
			landed, err = recoveryLandingHeads(ctx, filepath.Dir(runDir), record, route)
			if err != nil || len(landed) == 0 {
				return err
			}
		}
		url, err := recoveryRetentionCloneURL(setup.Config, record.RepositoryKey)
		if err != nil {
			return err
		}
		found, err := manager.WithRecoveryRepositories(ctx, url, func(repositories []string) error {
			if !eligible {
				eligible, err = verifyRecoveryLandingRepositories(ctx, repositories, record, landed)
				if err != nil || !eligible {
					return err
				}
			}
			if dryRun {
				pf(stdout, "retention candidate kind=recovery rule=recovery-policy run=%q ref=%q\n", record.RunID, record.Ref)
				return nil
			}
			_, err := recovery.RetireSnapshot(ctx, root, record, func(current recovery.Record) error {
				for _, repository := range repositories {
					if err := recovery.DeleteSnapshotRef(ctx, repository, current); err != nil {
						return err
					}
				}
				return nil
			})
			return err
		})
		if err == nil && !found {
			return fmt.Errorf("recovery retention requires an existing managed repository")
		}
		return err
	})
	return err
}

// A receiving commit may exist only in the pinned clone, not the mirror (or
// vice versa). One complete content proof suffices; an absent object in another
// managed copy must not hide that proof. Cleanup still checks every owned ref.
func verifyRecoveryLandingRepositories(ctx context.Context, repositories []string, record recovery.Record, landed []recovery.LandedHead) (bool, error) {
	var failures error
	for _, repository := range repositories {
		for _, head := range landed {
			verified, err := recovery.VerifyLandedRestoration(ctx, repository, record, head, 512<<20)
			if err != nil {
				failures = errors.Join(failures, err)
				continue
			}
			if verified {
				return true, nil
			}
		}
	}
	return false, failures
}

func recoveryRetentionOwner(runID string, managers []*worktree.Manager, runsByRoot map[string]string) (*worktree.Manager, string, error) {
	var owner *worktree.Manager
	var runDir string
	for _, manager := range managers {
		runsRoot, configured := runsByRoot[manager.Root]
		if !configured || runsRoot == "" {
			return nil, "", fmt.Errorf("recovery retention requires configured run directory mapping")
		}
		candidate := filepath.Join(runsRoot, runID)
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

func recoveryRetirementEligible(reader *journal.Reader, record recovery.Record, now time.Time, operatorEvents []journal.Event) (bool, error) {
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
	if len(operatorEvents) > 0 {
		abandoned, err := recovery.ExplicitlyAbandoned(operatorEvents, record)
		if err != nil || abandoned {
			return abandoned, err
		}
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
