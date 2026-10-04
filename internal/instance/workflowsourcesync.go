package instance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/credentials"
)

// PreparedConfigSwap is an installed workflow-source tree whose previous
// config directory remains available until the caller accepts or rejects the
// reload. Call exactly one of Commit or Rollback when Changed is true.
type PreparedConfigSwap struct {
	transaction *configTransaction
	finished    bool
	release     func() error
}

// PrepareGitWorkflowSourceIfChanged installs a changed source revision while
// retaining the prior config tree. This lets the daemon validate and apply the
// candidate, then roll the filesystem back if that revision is rejected.
func PrepareGitWorkflowSourceIfChanged(ctx context.Context, root string, source WorkflowSource, currentRevision string, appTokens GitTokenSource, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (revision string, changed bool, warnings []string, swap *PreparedConfigSwap, err error) {
	if err := RecoverConfigTransaction(NewLayout(root)); err != nil {
		return "", false, nil, nil, err
	}
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
	swap, err = PrepareConfigDirSwap(layout, stagedConfigDir)
	if err != nil {
		return "", false, nil, nil, err
	}
	return revision, true, warnings, swap, nil
}

// PrepareConfigDirSwap installs a durable candidate while retaining the previous
// generation for an explicit commit or rollback decision.
func PrepareConfigDirSwap(layout Layout, stagedConfigDir string) (*PreparedConfigSwap, error) {
	return prepareConfigTransaction(layout, stagedConfigDir, "", nil)
}

// Commit durably accepts the installed candidate before discarding its backup.
func (s *PreparedConfigSwap) Commit() error {
	if s == nil || s.finished {
		return nil
	}
	s.finished = true
	defer func() { _ = s.release() }()
	s.transaction.intent.Committed = true
	if err := s.transaction.writeIntent(); err != nil {
		return err
	}
	s.transaction.checkpoint("committed")
	return s.transaction.retire()
}

// Rollback restores the complete prior generation. Interrupted restores are
// replayed automatically using the immutable snapshot on the next attempt.
func (s *PreparedConfigSwap) Rollback() error {
	if s == nil || s.finished {
		return nil
	}
	s.finished = true
	defer func() { _ = s.release() }()
	if err := s.transaction.install("old"); err != nil {
		return err
	}
	return s.transaction.retire()
}
