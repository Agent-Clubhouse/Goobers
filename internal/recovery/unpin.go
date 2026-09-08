package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// DeleteSnapshotRef removes only the named recovery ref at its recorded SHA.
// This is the deletion mechanism, NOT an eligibility decision: the caller
// must verify repository ownership, terminal/paused protection and the durable
// retention/merge/abandonment policy while holding its lifecycle locks.
// It neither removes the archive nor runs Git garbage collection.
func DeleteSnapshotRef(ctx context.Context, repository string, record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	return deleteExactRecoveryRef(ctx, repository, record.Ref, record.SnapshotSHA)
}

func deleteExactRecoveryRef(ctx context.Context, repository, ref, expected string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// A symbolic alias is not the direct pin publication created. Refuse it
	// even if its target currently resolves to the expected commit. The
	// mutation itself also uses no-deref, protecting the target if an external
	// writer swaps in an alias between inspection and compare-and-delete.
	err := recoveryGit(ctx, repository, io.Discard, "symbolic-ref", "--quiet", ref)
	if err == nil {
		return ErrRecordConflict
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return fmt.Errorf("inspect recovery pin before deletion: %w", err)
	}
	if err := recoveryGit(ctx, repository, io.Discard, "update-ref", "--no-deref", "-d", ref, expected); err != nil {
		// A completed deletion may be retried after a crash. Verify absence
		// under Git's ref lock instead of treating every deletion error as
		// success (which would hide conflicting refs and repository failures).
		verify := "verify " + ref + " " + strings.Repeat("0", len(expected)) + "\n"
		if verifyErr := recoveryGitIO(ctx, repository, io.Discard, strings.NewReader(verify), nil, "update-ref", "--no-deref", "--stdin"); verifyErr == nil {
			return nil
		}
		return fmt.Errorf("compare-and-delete recovery pin: %w", err)
	}
	return nil
}
