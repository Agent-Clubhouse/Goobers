package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// FindRestoredAncestor finds an exact replay of record in the ancestry of a
// pinned receiving head. The caller must independently prove that this exact
// head landed in the same repository; this function does not prove a merge.
// Commit messages only narrow the bounded candidate search. Each candidate's
// complete tree is verified by replaying the retained patch onto its parent.
func FindRestoredAncestor(ctx context.Context, repository string, record Record, head string, maxPatchBytes int64) (string, error) {
	if err := record.Validate(); err != nil {
		return "", err
	}
	if !gitObjectID.MatchString(head) || maxPatchBytes <= 0 {
		return "", fmt.Errorf("recovery ancestry requires an exact receiving head and positive patch budget")
	}
	const maxCandidates = 128
	var output snapshotPathOutput
	marker := strings.SplitN(restorationMessage(record), "\n\n", 2)[1]
	if err := recoveryGit(ctx, repository, &output, "log", "--format=%H", "--max-count=129", "--fixed-strings", "--grep="+marker, head, "--"); err != nil {
		return "", err
	}
	candidates := strings.Fields(output.String())
	if len(candidates) > maxCandidates {
		return "", fmt.Errorf("recovery ancestry candidate budget exceeded")
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var message snapshotPathOutput
		if err := recoveryGit(ctx, repository, &message, "show", "--no-patch", "--format=%B", candidate); err != nil {
			return "", err
		}
		if strings.TrimSpace(message.String()) != restorationMessage(record) {
			continue
		}
		if err := VerifyRestoredCommit(ctx, repository, record, candidate, maxPatchBytes); err != nil {
			return "", fmt.Errorf("verify recovery ancestor: %w", err)
		}
		unchanged, err := retainedPathsUnchanged(ctx, repository, record, candidate, head)
		if err != nil {
			return "", err
		}
		if unchanged {
			return candidate, nil
		}
	}
	return "", nil
}

// A restored ancestor followed by a revert is not landed retained content.
// Compare every touched path literally, including deletions and both sides of
// renames. Changes outside those paths do not invalidate the retained patch.
func retainedPathsUnchanged(ctx context.Context, repository string, record Record, restored, head string) (bool, error) {
	var output snapshotPathOutput
	if err := recoveryGit(ctx, repository, &output, "diff", "--no-renames", "--name-only", "-z", record.BaseSHA, record.SnapshotSHA, "--"); err != nil {
		return false, err
	}
	paths := strings.Split(strings.TrimSuffix(output.String(), "\x00"), "\x00")
	if len(paths) == 1 && paths[0] == "" {
		return false, nil
	}
	args := append([]string{"--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--quiet", restored, head, "--"}, paths...)
	err := recoveryGit(ctx, repository, io.Discard, args...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}
