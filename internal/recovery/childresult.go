package recovery

import (
	"context"
	"fmt"
	"io"
	"time"
)

// CaptureChildResult preserves both committed and uncommitted child work.
// Its delta prerequisite is the recorded fork, never the child's final HEAD.
// The caller holds exclusive custody after every child process has joined.
func CaptureChildResult(ctx context.Context, repository, runID string, fork ChildSnapshot, finishedAt, retainUntil time.Time) (ChildSnapshot, error) {
	if err := fork.validate(); err != nil {
		return ChildSnapshot{}, err
	}
	if err := recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", fork.Record.SnapshotSHA, "HEAD"); err != nil {
		return ChildSnapshot{}, fmt.Errorf("child workspace no longer descends from recorded fork: %w", err)
	}
	result, err := CaptureChildSnapshot(ctx, repository, fork.Record.RepositoryKey, runID, finishedAt, retainUntil, fork.Policy)
	if err != nil {
		return ChildSnapshot{}, err
	}
	result.Record.BaseSHA = fork.Record.SnapshotSHA
	result.Record.BaseRef = fork.Record.SnapshotSHA
	result.Record.PatchDigest, err = WriteSnapshotPatch(ctx, repository, result.Record.BaseSHA, result.Record.SnapshotSHA, io.Discard)
	return result, err
}
