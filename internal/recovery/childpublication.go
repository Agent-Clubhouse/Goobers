package recovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// CaptureChildPublication produces a normal descendant commit from the managed
// fork. Omitted paths retain their original repository versions; private runtime
// or injected files cannot become deletions or contents in the published tree.
// It never changes HEAD, the real index, branch, or working files. The caller
// must own the workspace and durably retain the returned intent before push.
func CaptureChildPublication(ctx context.Context, repository, runID string, fork ChildSnapshot, identityTime time.Time) (ChildSnapshot, string, error) {
	if err := fork.validate(); err != nil {
		return ChildSnapshot{}, "", err
	}
	if err := recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", fork.Record.SnapshotSHA, "HEAD"); err != nil {
		return ChildSnapshot{}, "", fmt.Errorf("child publication no longer descends from admitted fork: %w", err)
	}
	snapshot, err := CaptureChildSnapshot(ctx, repository, fork.Record.RepositoryKey, runID, identityTime, fork.Record.RetainUntil, fork.Policy)
	if err != nil {
		return ChildSnapshot{}, "", err
	}
	directory, err := os.MkdirTemp("", "goobers-child-publication-*")
	if err != nil {
		return ChildSnapshot{}, "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	env, err := snapshotEnvironment(ctx, repository, directory, snapshot.Record.BaseSHA)
	if err != nil {
		return ChildSnapshot{}, "", err
	}
	if err = recoveryGitWithEnv(ctx, repository, io.Discard, env, "read-tree", snapshot.TreeSHA); err != nil {
		return ChildSnapshot{}, "", err
	}
	if err = restorePublicationOmissions(ctx, repository, env, fork); err != nil {
		return ChildSnapshot{}, "", err
	}
	var tree, commit boundedRefOutput
	if err = recoveryGitWithEnv(ctx, repository, &tree, env, "write-tree"); err != nil {
		return ChildSnapshot{}, "", err
	}
	treeID := strings.TrimSpace(tree.String())
	if !gitObjectID.MatchString(treeID) {
		return ChildSnapshot{}, "", fmt.Errorf("invalid publication tree")
	}
	date := identityTime.UTC().Format(time.RFC3339)
	env = append(env, "GIT_AUTHOR_NAME=Goobers", "GIT_AUTHOR_EMAIL=goobers@goobers.invalid", "GIT_COMMITTER_NAME=Goobers", "GIT_COMMITTER_EMAIL=goobers@goobers.invalid", "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if err = recoveryGitWithEnv(ctx, repository, &commit, env, "-c", "commit.gpgsign=false", "commit-tree", treeID, "-p", snapshot.Record.BaseSHA, "-m", "Publish Goobers child "+runID+"\n\nAccepted: "+identityTime.UTC().Format(time.RFC3339Nano)); err != nil {
		return ChildSnapshot{}, "", err
	}
	sha := strings.TrimSpace(commit.String())
	if !gitObjectID.MatchString(sha) {
		return ChildSnapshot{}, "", fmt.Errorf("invalid publication commit")
	}
	return snapshot, sha, nil
}

func restorePublicationOmissions(ctx context.Context, repository string, env []string, fork ChildSnapshot) error {
	var entries bytes.Buffer
	bounded := &archiveBudgetWriter{destination: &entries, remaining: maxSnapshotIndexBytes}
	if err := recoveryGit(ctx, repository, bounded, "ls-tree", "-rz", "--full-tree", fork.Record.BaseSHA); err != nil {
		return err
	}
	var omitted bytes.Buffer
	for entries.Len() > 0 {
		entry, rest, ok := bytes.Cut(entries.Bytes(), []byte{0})
		if !ok {
			return fmt.Errorf("invalid original publication tree")
		}
		_, name, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return fmt.Errorf("invalid original publication entry")
		}
		if fork.Policy.excludes(string(name)) {
			_, _ = omitted.Write(entry)
			_ = omitted.WriteByte(0)
		}
		entries.Next(entries.Len() - len(rest))
	}
	if omitted.Len() == 0 {
		return nil
	}
	return recoveryGitIO(ctx, repository, io.Discard, &omitted, env, "update-index", "-z", "--index-info")
}
