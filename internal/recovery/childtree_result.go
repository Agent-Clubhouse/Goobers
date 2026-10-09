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
	date := at.UTC().Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_NAME=Goobers Recovery", "GIT_AUTHOR_EMAIL=recovery@goobers.invalid", "GIT_COMMITTER_NAME=Goobers Recovery", "GIT_COMMITTER_EMAIL=recovery@goobers.invalid", "GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	message := "Filtered branch result for " + runID + "\n\nCapture identity: " + at.UTC().Format(time.RFC3339Nano)
	var output boundedRefOutput
	if err := recoveryGitWithEnv(ctx, repository, &output, env, "-c", "commit.gpgsign=false", "commit-tree", result.TreeSHA, "-p", fork.Record.SnapshotSHA, "-m", message); err != nil {
		return ChildSnapshot{}, err
	}
	result.Record.SnapshotSHA = strings.TrimSpace(output.String())
	if !gitObjectID.MatchString(result.Record.SnapshotSHA) {
		return ChildSnapshot{}, fmt.Errorf("invalid filtered branch result commit")
	}
	result.Record.Ref, err = RefForSnapshot(runID, result.Record.SnapshotSHA)
	if err != nil {
		return ChildSnapshot{}, err
	}
	result.Record.PatchDigest, err = WriteSnapshotPatch(ctx, repository, result.Record.BaseSHA, result.Record.SnapshotSHA, io.Discard)
	return result, err
}
