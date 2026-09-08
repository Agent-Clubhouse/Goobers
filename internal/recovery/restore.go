package recovery

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RestoreSnapshot applies the complete retained base-to-snapshot patch onto
// currentMain and creates a new branch by compare-and-swap. The caller must
// resolve/verify currentMain against the current authoritative main before
// calling, and import the independently verified archive objects if needed.
// This function neither fetches a moving remote main nor mutates the current
// checkout/index. Conflicts leave no new branch; retained recovery is untouched.
func RestoreSnapshot(ctx context.Context, repository string, record Record, currentMain, branch string, maxPatchBytes int64) (string, error) {
	if err := record.Validate(); err != nil {
		return "", err
	}
	if maxPatchBytes <= 0 || !gitObjectID.MatchString(currentMain) {
		return "", fmt.Errorf("restore requires a valid main commit and positive patch budget")
	}
	ref := "refs/heads/" + branch
	if err := recoveryGit(ctx, repository, io.Discard, "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("invalid recovery branch name")
	}
	if err := PinCommit(ctx, repository, record); err != nil {
		return "", err
	}
	if err := validateRestorePaths(ctx, repository, record); err != nil {
		return "", err
	}
	if err := recoveryGit(ctx, repository, io.Discard, "cat-file", "-e", currentMain+"^{commit}"); err != nil {
		return "", fmt.Errorf("current main commit is unavailable: %w", err)
	}
	directory, err := os.MkdirTemp("", "goobers-recovery-restore-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	environment, err := snapshotEnvironment(ctx, repository, directory, currentMain)
	if err != nil {
		return "", err
	}
	if err := recoveryGitWithEnv(ctx, repository, io.Discard, environment, "read-tree", currentMain); err != nil {
		return "", err
	}
	if err := applyRetainedPatch(ctx, repository, directory, environment, record, maxPatchBytes); err != nil {
		return "", err
	}
	var tree, commit boundedRefOutput
	if err := recoveryGitWithEnv(ctx, repository, &tree, environment, "write-tree"); err != nil {
		return "", fmt.Errorf("write restored tree: %w", err)
	}
	environment = append(environment,
		"GIT_AUTHOR_NAME=Goobers Recovery", "GIT_AUTHOR_EMAIL=recovery@goobers.invalid",
		"GIT_COMMITTER_NAME=Goobers Recovery", "GIT_COMMITTER_EMAIL=recovery@goobers.invalid")
	if err := recoveryGitWithEnv(ctx, repository, &commit, environment,
		"-c", "commit.gpgsign=false", "commit-tree", strings.TrimSpace(tree.String()), "-p", currentMain,
		"-m", "Restore retained implementation for "+record.RunID); err != nil {
		return "", fmt.Errorf("commit restored patch: %w", err)
	}
	id := strings.TrimSpace(commit.String())
	if !gitObjectID.MatchString(id) {
		return "", fmt.Errorf("invalid restored commit identity")
	}
	if err := recoveryGit(ctx, repository, io.Discard, "update-ref", "--no-deref", ref, id, strings.Repeat("0", len(id))); err != nil {
		return "", fmt.Errorf("create recovery branch without overwrite: %w", err)
	}
	return id, nil
}

func applyRetainedPatch(ctx context.Context, repository, directory string, environment []string, record Record, budget int64) error {
	file, err := os.OpenFile(filepath.Join(directory, "retained.patch"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	digest, err := WriteSnapshotPatch(ctx, repository, record.BaseSHA, record.SnapshotSHA, &archiveBudgetWriter{destination: file, remaining: budget})
	if err != nil {
		return err
	}
	if digest != record.PatchDigest {
		return fmt.Errorf("retained patch digest changed before restore")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := recoveryGitIO(ctx, repository, io.Discard, file, environment,
		"apply", "--cached", "--3way", "--binary", "--whitespace=nowarn", "-"); err != nil {
		return fmt.Errorf("retained patch cannot be applied cleanly to current main: %w", err)
	}
	return nil
}
