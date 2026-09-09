package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// PreparedRestoreBranch is the receiving run's temporary preparation branch.
func PreparedRestoreBranch(runID string) (string, error) {
	if !runIdentity.MatchString(runID) {
		return "", fmt.Errorf("invalid receiving run identity")
	}
	return "goobers/recovery-resume/" + runID, nil
}

// FindPreparedRestore verifies a prior preparation or completed adoption.
// The caller must first import the verified archive and exclusively own the
// receiving worktree. An existing preparation is never silently replaced.
func FindPreparedRestore(ctx context.Context, repository string, record Record, runID string, maxPatchBytes int64) (string, error) {
	branch, err := PreparedRestoreBranch(runID)
	if err != nil {
		return "", err
	}
	var output boundedRefOutput
	ref := "refs/heads/" + branch
	err = recoveryGit(ctx, repository, io.Discard, "show-ref", "--verify", "--quiet", ref)
	prepared := err == nil
	if prepared {
		if err := recoveryGit(ctx, repository, &output, "rev-parse", "--verify", ref+"^{commit}"); err != nil {
			return "", err
		}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return "", fmt.Errorf("inspect prepared restoration: %w", err)
		}
		output.Reset()
		if err := recoveryGit(ctx, repository, &output, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
			return "", err
		}
	}
	commit := strings.TrimSpace(output.String())
	if !gitObjectID.MatchString(commit) {
		return "", fmt.Errorf("invalid prepared restoration commit")
	}
	var message snapshotPathOutput
	if err := recoveryGit(ctx, repository, &message, "show", "--no-patch", "--format=%B", commit); err != nil {
		return "", err
	}
	if !matchesRestorationMessage(record, message.String()) {
		if prepared {
			return "", fmt.Errorf("prepared branch belongs to different recovery state")
		}
		return "", nil
	}
	if err := VerifyRestoredCommit(ctx, repository, record, commit, maxPatchBytes); err != nil {
		return "", err
	}
	return commit, nil
}

// DeletePreparedRestore removes only this run's preparation at the adopted
// commit. Call only after adoption has pinned the result on the receiving
// branch. The receiving commit itself carries the verified replay evidence.
func DeletePreparedRestore(ctx context.Context, repository, runID, commit string) error {
	branch, err := PreparedRestoreBranch(runID)
	if err != nil {
		return err
	}
	if !gitObjectID.MatchString(commit) {
		return fmt.Errorf("invalid adopted restoration commit")
	}
	return deleteExactRecoveryRef(ctx, repository, "refs/heads/"+branch, commit)
}
