package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/credentials"
)

// SyncGitWorkflowSource resolves a git-tracked workflowSource to its latest
// committed revision and atomically installs its definitions
// (manifest.yaml + gaggles/) into the runtime config directory, replacing
// whatever was there (#459). It does not itself decide whether the newly
// installed definitions are valid — the caller's existing config-reload path
// owns that, exactly as it already does for a human hand-editing files in
// place: a rejected reload leaves the installed-but-invalid files on disk
// with the daemon still running its prior definitions. That is this
// package's existing "last-known-good" contract, not a new one invented
// here.
//
// Returns the resolved revision (the tracked ref's commit sha) on success.
// appTokens is the installation-token minting source for auth kind github-app
// (#3274), nil for every other source shape — see NewWorkflowGitSource.
func SyncGitWorkflowSource(ctx context.Context, root string, source WorkflowSource, appTokens GitTokenSource, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (revision string, warnings []string, err error) {
	revision, _, warnings, err = SyncGitWorkflowSourceIfChanged(ctx, root, source, "", appTokens, registrar, stores)
	return revision, warnings, err
}

// SyncGitWorkflowSourceIfChanged resolves the tracked Git ref and installs its
// definitions only when it differs from currentRevision.
func SyncGitWorkflowSourceIfChanged(ctx context.Context, root string, source WorkflowSource, currentRevision string, appTokens GitTokenSource, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (revision string, changed bool, warnings []string, err error) {
	revision, changed, warnings, swap, err := PrepareGitWorkflowSourceIfChanged(ctx, root, source, currentRevision, appTokens, registrar, stores)
	if err != nil || swap == nil {
		return revision, changed, warnings, err
	}
	if err := swap.Commit(); err != nil {
		return "", false, warnings, err
	}
	return revision, changed, warnings, nil
}

// PreparedConfigSwap is an installed workflow-source tree whose previous
// config directory remains available until the caller accepts or rejects the
// reload. Call exactly one of Commit or Rollback when Changed is true.
type PreparedConfigSwap struct {
	layout       Layout
	backupRoot   string
	backupConfig string
	finished     bool
}

// PrepareGitWorkflowSourceIfChanged installs a changed source revision while
// retaining the prior config tree. This lets the daemon validate and apply the
// candidate, then roll the filesystem back if that revision is rejected.
func PrepareGitWorkflowSourceIfChanged(ctx context.Context, root string, source WorkflowSource, currentRevision string, appTokens GitTokenSource, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (revision string, changed bool, warnings []string, swap *PreparedConfigSwap, err error) {
	if source.Kind != WorkflowSourceKindGit {
		return "", false, nil, nil, fmt.Errorf("sync workflow source: kind %q is not %q", source.Kind, WorkflowSourceKindGit)
	}
	gitSource, err := NewWorkflowGitSource(root, source, appTokens, registrar, stores)
	if err != nil {
		return "", false, nil, nil, err
	}
	snapshot, err := gitSource.Resolve(ctx)
	if err != nil {
		return "", false, nil, nil, err
	}
	revision = filepath.Base(snapshot)
	warnings = gitSource.Warnings()
	if revision == currentRevision {
		return revision, false, warnings, nil, nil
	}

	layout := NewLayout(root)
	stagingRoot, err := os.MkdirTemp(root, ".config-apply-")
	if err != nil {
		return "", false, nil, nil, fmt.Errorf("create config apply staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingRoot) }()

	stagedConfigDir := filepath.Join(stagingRoot, ConfigDirName)
	if err := copyGuidedSourceDefinitions(stagedConfigDir, snapshot); err != nil {
		return "", false, nil, nil, fmt.Errorf("stage git workflow source: %w", err)
	}
	swap, err = prepareSyncedConfigDir(layout, stagedConfigDir)
	if err != nil {
		return "", false, nil, nil, err
	}
	return revision, true, warnings, swap, nil
}

// prepareSyncedConfigDir atomically replaces layout.ConfigDir() while retaining
// the previous directory for an explicit commit or rollback decision.
func prepareSyncedConfigDir(layout Layout, stagedConfigDir string) (*PreparedConfigSwap, error) {
	backupRoot, err := os.MkdirTemp(layout.Root, ".config-apply-backup-")
	if err != nil {
		return nil, fmt.Errorf("create config apply backup directory: %w", err)
	}
	backupConfigDir := filepath.Join(backupRoot, ConfigDirName)
	if err := os.Rename(layout.ConfigDir(), backupConfigDir); err != nil {
		_ = os.RemoveAll(backupRoot)
		return nil, fmt.Errorf("back up %s: %w", ConfigDirName, err)
	}
	if err := os.Rename(stagedConfigDir, layout.ConfigDir()); err != nil {
		rollbackErr := os.Rename(backupConfigDir, layout.ConfigDir())
		return nil, errors.Join(
			fmt.Errorf("install %s: %w", ConfigDirName, err),
			rollbackErr,
			os.RemoveAll(backupRoot),
		)
	}
	return &PreparedConfigSwap{layout: layout, backupRoot: backupRoot, backupConfig: backupConfigDir}, nil
}

// Commit accepts the installed candidate and removes its prior-tree backup.
func (s *PreparedConfigSwap) Commit() error {
	if s == nil || s.finished {
		return nil
	}
	if err := os.RemoveAll(s.backupRoot); err != nil {
		return fmt.Errorf("remove config apply backup %s: %w", s.backupRoot, err)
	}
	s.finished = true
	return nil
}

// Rollback atomically restores the prior config tree and then removes the
// rejected candidate. A failed restore attempts to put the candidate back so
// the instance never deliberately loses both generations.
func (s *PreparedConfigSwap) Rollback() error {
	if s == nil || s.finished {
		return nil
	}
	rejectedRoot, err := os.MkdirTemp(s.layout.Root, ".config-rejected-")
	if err != nil {
		return fmt.Errorf("create rejected config staging directory: %w", err)
	}
	rejectedConfig := filepath.Join(rejectedRoot, ConfigDirName)
	if err := os.Rename(s.layout.ConfigDir(), rejectedConfig); err != nil {
		_ = os.RemoveAll(rejectedRoot)
		return fmt.Errorf("stage rejected %s: %w", ConfigDirName, err)
	}
	if err := os.Rename(s.backupConfig, s.layout.ConfigDir()); err != nil {
		reinstateErr := os.Rename(rejectedConfig, s.layout.ConfigDir())
		return errors.Join(
			fmt.Errorf("restore applied %s: %w", ConfigDirName, err),
			reinstateErr,
			os.RemoveAll(rejectedRoot),
		)
	}
	s.finished = true
	return errors.Join(os.RemoveAll(rejectedRoot), os.RemoveAll(s.backupRoot))
}
