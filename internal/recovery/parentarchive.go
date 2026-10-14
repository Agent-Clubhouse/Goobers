package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const parentArchivePrefix = "Goobers retained parent Git state v1\n\n"

// RetainedParentState describes the independent trees inside one ordinary
// recovery archive. The snapshot's first parent is the original host HEAD;
// its second is a filtered index root. Both are reachable from the retained
// ref, so existing bundle, inventory, overflow and retirement rules protect them.
type RetainedParentState struct {
	HeadSHA  string
	IndexSHA string
	TreeSHA  string
	Policy   SnapshotPolicy
}

func prepareRetentionRecord(ctx context.Context, request RetentionRequest) (Record, error) {
	if request.ParentPolicy == nil {
		return PrepareRecord(ctx, request.Repository, request.RepositoryKey, request.RunID, request.BaseRef, request.IdentityTime, request.RetainUntil)
	}
	prepared, err := prepareRecordBase(ctx, request.Repository, request.RepositoryKey, request.RunID, request.BaseRef, request.IdentityTime, request.RetainUntil)
	if err != nil {
		return Record{}, err
	}
	snapshot, err := CaptureChildSnapshot(ctx, request.Repository, request.RepositoryKey, request.RunID, request.IdentityTime, request.RetainUntil, *request.ParentPolicy)
	if err != nil {
		return Record{}, err
	}
	index, err := parentArchiveIndex(ctx, request.Repository, snapshot)
	if err != nil {
		return Record{}, err
	}
	commit, err := parentArchiveCommit(ctx, request.Repository, snapshot, index)
	if err != nil {
		return Record{}, err
	}
	prepared, err = completePreparedRecord(ctx, request.Repository, prepared, commit)
	if err != nil {
		return Record{}, err
	}
	if err := CheckChildSnapshotCurrent(ctx, request.Repository, snapshot); err != nil {
		return Record{}, err
	}
	if _, err := ReadRetainedParentState(ctx, request.Repository, prepared); err != nil {
		return Record{}, err
	}
	if err := verifyParentArchiveApplication(ctx, request.Repository, snapshot, request.MaxArchiveBytes); err != nil {
		return Record{}, err
	}
	return prepared, nil
}

// A valid bundle alone is not permission to discard the checkout. Ordinary
// stages can produce more work than the bounded restore protocol supports, or
// change file/directory shapes it cannot replay. Check both independent trees
// against the fresh checkout's HEAD before Retain acknowledges cleanup. This
// reads objects and private indexes only; the current checkout stays untouched.
func verifyParentArchiveApplication(ctx context.Context, repository string, snapshot ChildSnapshot, maxBytes int64) error {
	head, err := WritePortableGitState(ctx, repository, snapshot, false, io.Discard, maxBytes)
	if err != nil {
		return err
	}
	index, err := WritePortableGitState(ctx, repository, snapshot, true, io.Discard, maxBytes)
	if err != nil {
		return err
	}
	if _, err := writePortableTree(ctx, repository, snapshot, snapshot.TreeSHA, io.Discard, maxBytes); err != nil {
		return err
	}
	expected := snapshot
	expected.TreeSHA = head.TreeSHA
	for _, tree := range []string{snapshot.TreeSHA, index.TreeSHA} {
		if _, err := loadChildChanges(ctx, repository, PreparedChildDisposition{ExpectedParent: expected, TreeSHA: tree}); err != nil {
			return fmt.Errorf("parent archive cannot be restored automatically: %w", err)
		}
	}
	return CheckChildSnapshotCurrent(ctx, repository, snapshot)
}

func parentArchiveIndex(ctx context.Context, repository string, snapshot ChildSnapshot) (string, error) {
	directory, err := privateGitDirectory(ctx, repository, "goobers-parent-archive-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	env, err := snapshotEnvironment(ctx, repository, directory, snapshot.Record.BaseSHA)
	if err != nil {
		return "", err
	}
	if err := snapshotIndexWithPolicy(ctx, repository, env, &snapshot.Policy); err != nil {
		return "", err
	}
	snapshot.TreeSHA, err = privateIndexTree(ctx, repository, env)
	if err != nil {
		return "", err
	}
	return portableRoot(ctx, repository, snapshot)
}

func parentArchiveCommit(ctx context.Context, repository string, snapshot ChildSnapshot, index string) (string, error) {
	tree, err := parentArchiveWorkingTree(ctx, repository, snapshot)
	if err != nil {
		return "", err
	}
	policy, err := json.Marshal(snapshot.Policy)
	if err != nil {
		return "", err
	}
	if len(policy) > (128<<10)-1024 {
		return "", fmt.Errorf("parent archive policy exceeds metadata budget")
	}
	date := snapshot.Record.CreatedAt.UTC().Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_NAME=Goobers Recovery", "GIT_AUTHOR_EMAIL=recovery@goobers.invalid", "GIT_COMMITTER_NAME=Goobers Recovery", "GIT_COMMITTER_EMAIL=recovery@goobers.invalid", "GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	var output boundedRefOutput
	if err := recoveryGitWithEnv(ctx, repository, &output, env, "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", snapshot.Record.BaseSHA, "-p", index, "-m", parentArchivePrefix+string(policy)); err != nil {
		return "", err
	}
	commit := strings.TrimSpace(output.String())
	if !gitObjectID.MatchString(commit) {
		return "", fmt.Errorf("invalid parent archive commit")
	}
	return commit, nil
}

// Ordinary recovery consumers see a cumulative implementation tree. Restore
// omitted paths from original HEAD so an exclusion cannot become a deletion
// when that tree is applied onto a new checkout. Runtime/staged replacements
// never enter this tree; the independent index root remains filtered.
func parentArchiveWorkingTree(ctx context.Context, repository string, snapshot ChildSnapshot) (string, error) {
	directory, err := privateGitDirectory(ctx, repository, "goobers-parent-working-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	env, err := snapshotEnvironment(ctx, repository, directory, snapshot.Record.BaseSHA)
	if err != nil {
		return "", err
	}
	if err := recoveryGitWithEnv(ctx, repository, io.Discard, env, "read-tree", snapshot.TreeSHA); err != nil {
		return "", err
	}
	if err := restorePublicationOmissions(ctx, repository, env, snapshot); err != nil {
		return "", err
	}
	return privateIndexTree(ctx, repository, env)
}

// ReadRetainedParentState verifies the extra state after ordinary archive import
// has verified its digest, base, snapshot and patch. It changes no live files.
// This host-only archive includes real repository history and must never be
// delivered through the filtered worker workspace transport.
func ReadRetainedParentState(ctx context.Context, repository string, record Record) (RetainedParentState, error) {
	var result RetainedParentState
	if err := record.ValidateRestorable(); err != nil {
		return result, err
	}
	var metadata bytes.Buffer
	bounded := &archiveBudgetWriter{destination: &metadata, remaining: 128 << 10}
	if err := recoveryGit(ctx, repository, bounded, "show", "-s", "--format=%P%n%B", record.SnapshotSHA); err != nil {
		return result, err
	}
	parents, message, ok := strings.Cut(metadata.String(), "\n")
	fields := strings.Fields(parents)
	if !ok || len(fields) != 2 || !gitObjectID.MatchString(fields[0]) || !gitObjectID.MatchString(fields[1]) || !strings.HasPrefix(message, parentArchivePrefix) {
		return result, fmt.Errorf("recovery snapshot is not a retained parent Git state")
	}
	data := []byte(strings.TrimSuffix(strings.TrimPrefix(message, parentArchivePrefix), "\n\n"))
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result.Policy); err != nil {
		return result, err
	}
	canonical, err := json.Marshal(result.Policy)
	if err != nil || !bytes.Equal(canonical, data) {
		return result, fmt.Errorf("noncanonical parent archive policy")
	}
	if err := result.Policy.Validate(); err != nil {
		return result, err
	}
	result.HeadSHA, result.IndexSHA = fields[0], fields[1]
	result.TreeSHA, err = snapshotObject(ctx, repository, record.SnapshotSHA+"^{tree}")
	if err != nil {
		return result, err
	}
	if err := verifyParentArchiveObjects(ctx, repository, record, result); err != nil {
		return RetainedParentState{}, err
	}
	return result, nil
}

func verifyParentArchiveObjects(ctx context.Context, repository string, record Record, state RetainedParentState) error {
	var parents boundedRefOutput
	if err := recoveryGit(ctx, repository, &parents, "rev-list", "--parents", "-n", "1", state.IndexSHA); err != nil {
		return err
	}
	if strings.TrimSpace(parents.String()) != state.IndexSHA {
		return fmt.Errorf("parent archive index contains unexpected ancestry")
	}
	if err := recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", record.BaseSHA, state.HeadSHA); err != nil {
		return fmt.Errorf("parent archive HEAD differs from cumulative base: %w", err)
	}
	if err := verifySnapshotPolicy(ctx, repository, state.IndexSHA, state.Policy); err != nil {
		return err
	}
	return verifyParentArchiveOmissions(ctx, repository, state)
}

func verifyParentArchiveOmissions(ctx context.Context, repository string, state RetainedParentState) error {
	head, err := childTree(ctx, repository, state.HeadSHA)
	if err != nil {
		return err
	}
	working, err := childTree(ctx, repository, state.TreeSHA)
	if err != nil {
		return err
	}
	for _, entries := range []map[string]childTreeEntry{head, working} {
		for name := range entries {
			if state.Policy.excludes(name) && head[name] != working[name] {
				return fmt.Errorf("parent archive changed an omitted repository path")
			}
		}
	}
	return nil
}
