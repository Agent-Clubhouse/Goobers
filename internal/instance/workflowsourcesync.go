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
	if source.Kind != WorkflowSourceKindGit {
		return "", false, nil, fmt.Errorf("sync workflow source: kind %q is not %q", source.Kind, WorkflowSourceKindGit)
	}
	gitSource, err := NewWorkflowGitSource(root, source, appTokens, registrar, stores)
	if err != nil {
		return "", false, nil, err
	}
	snapshot, err := gitSource.Resolve(ctx)
	if err != nil {
		return "", false, nil, err
	}
	revision = filepath.Base(snapshot)
	warnings = gitSource.Warnings()
	if revision == currentRevision {
		return revision, false, warnings, nil
	}

	layout := NewLayout(root)
	stagingRoot, err := os.MkdirTemp(root, ".config-apply-")
	if err != nil {
		return "", false, nil, fmt.Errorf("create config apply staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingRoot) }()

	stagedConfigDir := filepath.Join(stagingRoot, ConfigDirName)
	if err := copyGuidedSourceDefinitions(stagedConfigDir, snapshot); err != nil {
		return "", false, nil, fmt.Errorf("stage git workflow source: %w", err)
	}
	if err := installSyncedConfigDir(layout, stagedConfigDir); err != nil {
		return "", false, nil, err
	}
	return revision, true, warnings, nil
}

// installSyncedConfigDir atomically replaces layout.ConfigDir() with
// stagedConfigDir, backing up the previous directory so a failed second
// rename can roll back rather than leave the instance with neither. Mirrors
// installMaterializedConfig's rename-swap idiom, scoped to just the config
// directory since a git workflow source never touches instance.yaml.
func installSyncedConfigDir(layout Layout, stagedConfigDir string) error {
	backupRoot, err := os.MkdirTemp(layout.Root, ".config-apply-backup-")
	if err != nil {
		return fmt.Errorf("create config apply backup directory: %w", err)
	}
	backupConfigDir := filepath.Join(backupRoot, ConfigDirName)
	if err := os.Rename(layout.ConfigDir(), backupConfigDir); err != nil {
		_ = os.RemoveAll(backupRoot)
		return fmt.Errorf("back up %s: %w", ConfigDirName, err)
	}
	if err := os.Rename(stagedConfigDir, layout.ConfigDir()); err != nil {
		rollbackErr := os.Rename(backupConfigDir, layout.ConfigDir())
		return errors.Join(
			fmt.Errorf("install %s: %w", ConfigDirName, err),
			rollbackErr,
			os.RemoveAll(backupRoot),
		)
	}
	if err := os.RemoveAll(backupRoot); err != nil {
		return fmt.Errorf("remove config apply backup %s: %w", backupRoot, err)
	}
	return nil
}
