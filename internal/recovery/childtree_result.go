package recovery

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// CaptureChildTreeResult captures allowed committed and uncommitted changes on
// a single synthetic commit rooted at the original fork. The resulting delta
// cannot carry intermediate branch history containing policy-excluded paths.
// HEAD, index and working files remain unchanged; custody must be exclusive.
func CaptureChildTreeResult(ctx context.Context, repository, runID string, fork ChildSnapshot, at, retainUntil time.Time) (ChildSnapshot, error) {
	result, err := CaptureChildResult(ctx, repository, runID, fork, at, retainUntil)
	if err != nil {
		return ChildSnapshot{}, err
	}
	result.Record.SnapshotSHA, err = commitChildResultTree(ctx, repository, result.TreeSHA, fork.Record.SnapshotSHA, runID, at, nil)
	if err != nil {
		return ChildSnapshot{}, err
	}
	result.Record.Ref, err = RefForSnapshot(runID, result.Record.SnapshotSHA)
	if err != nil {
		return ChildSnapshot{}, err
	}
	result.Record.PatchDigest, err = WriteSnapshotPatch(ctx, repository, result.Record.BaseSHA, result.Record.SnapshotSHA, io.Discard)
	return result, err
}

// Creates only immutable objects. The caller must record provenance before any
// pin or carrier publication; no temporary ref escapes this preparation.
func commitChildResultTree(ctx context.Context, repository, tree, parent, runID string, at time.Time, inputs []string) (string, error) {
	date := at.UTC().Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_NAME=Goobers Recovery", "GIT_AUTHOR_EMAIL=recovery@goobers.invalid", "GIT_COMMITTER_NAME=Goobers Recovery", "GIT_COMMITTER_EMAIL=recovery@goobers.invalid", "GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	message := "Filtered branch result for " + runID + "\n\nCapture identity: " + at.UTC().Format(time.RFC3339Nano)
	if len(inputs) != 0 {
		message += "\nOrdered results: " + strings.Join(inputs, " ")
	}
	var output boundedRefOutput
	if err := recoveryGitWithEnv(ctx, repository, &output, env, "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", parent, "-m", message); err != nil {
		return "", err
	}
	commit := strings.TrimSpace(output.String())
	if !gitObjectID.MatchString(commit) {
		return "", fmt.Errorf("invalid filtered branch result commit")
	}
	return commit, nil
}
