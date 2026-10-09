package recovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
)

// PlanRetainedParentRestore prepares exact Git-state restoration in a newly
// provisioned host checkout at the archived original HEAD. The caller imports
// the verified archive first, owns the checkout exclusively, then durably stores
// this plan before ApplyChildApplication. Interrupted application must reuse the
// saved plan, never replan against partially restored state. Real ancestry and
// omitted source paths survive; synthetic archive/index roots never become HEAD.
func PlanRetainedParentRestore(ctx context.Context, repository string, record Record, operation string, maxBytes int64) (ChildApplyPlan, error) {
	state, err := ReadRetainedParentState(ctx, repository, record)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	if err := PinCommit(ctx, repository, record); err != nil {
		return ChildApplyPlan{}, err
	}
	if err := requireParentRestoreCheckout(ctx, repository, state.HeadSHA); err != nil {
		return ChildApplyPlan{}, err
	}
	expected, err := CaptureChildSnapshot(ctx, repository, record.RepositoryKey, record.RunID, record.CreatedAt, record.RetainUntil, state.Policy)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	head, err := WritePortableGitState(ctx, repository, expected, false, io.Discard, maxBytes)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	index, err := WritePortableGitState(ctx, repository, expected, true, io.Discard, maxBytes)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	working, err := portableParentRevision(ctx, repository, expected, record.SnapshotSHA, maxBytes)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	staged, err := portableParentRevision(ctx, repository, expected, state.IndexSHA, maxBytes)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	before := ParentGitState{Head: head, Index: index}
	after := ParentGitState{Head: head, Index: staged}
	return PrepareParentApplication(ctx, repository, expected, before, after, working, operation, record.CreatedAt, maxBytes)
}

// VerifyRetainedParentCheckout recognizes a surviving checkout after a crash
// between retirement acknowledgement and removal, or after completed apply.
// Matching HEAD alone is insufficient: the index and working tree must both
// match the retained state under its original exclusion policy.
func VerifyRetainedParentCheckout(ctx context.Context, repository string, record Record, maxBytes int64) error {
	state, err := ReadRetainedParentState(ctx, repository, record)
	if err != nil {
		return err
	}
	current, err := CaptureChildSnapshot(ctx, repository, record.RepositoryKey, record.RunID, record.CreatedAt, record.RetainUntil, state.Policy)
	if err != nil {
		return err
	}
	if current.Record.BaseSHA != state.HeadSHA {
		return ErrWorkspaceChanged
	}
	index, err := WritePortableGitState(ctx, repository, current, true, io.Discard, maxBytes)
	if err != nil {
		return err
	}
	staged, err := snapshotObject(ctx, repository, state.IndexSHA+"^{tree}")
	if err != nil {
		return err
	}
	working, err := portableParentRevision(ctx, repository, current, record.SnapshotSHA, maxBytes)
	if err != nil {
		return err
	}
	if index.TreeSHA != staged || current.TreeSHA != working.TreeSHA {
		return ErrWorkspaceChanged
	}
	return CheckChildSnapshotCurrent(ctx, repository, current)
}

func requireParentRestoreCheckout(ctx context.Context, repository, head string) error {
	current, err := snapshotObject(ctx, repository, "HEAD^{commit}")
	if err != nil {
		return err
	}
	if current != head {
		return fmt.Errorf("parent restore requires its exact original HEAD")
	}
	var status bytes.Buffer
	bounded := &archiveBudgetWriter{destination: &status, remaining: maxSnapshotIndexBytes}
	if err := recoveryGit(ctx, repository, bounded, "status", "--porcelain=v1", "--untracked-files=all", "-z"); err != nil {
		return err
	}
	if status.Len() != 0 {
		return fmt.Errorf("parent restore requires a clean newly provisioned checkout")
	}
	return nil
}

func portableParentRevision(ctx context.Context, repository string, expected ChildSnapshot, revision string, maxBytes int64) (PortableSnapshot, error) {
	directory, err := privateGitDirectory(ctx, repository, "goobers-parent-restore-*")
	if err != nil {
		return PortableSnapshot{}, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	env, err := snapshotEnvironment(ctx, repository, directory, expected.Record.BaseSHA)
	if err != nil {
		return PortableSnapshot{}, err
	}
	if err := readFilteredRevisionIndex(ctx, repository, env, revision, expected.Policy); err != nil {
		return PortableSnapshot{}, err
	}
	tree, err := privateIndexTree(ctx, repository, env)
	if err != nil {
		return PortableSnapshot{}, err
	}
	return writePortableTree(ctx, repository, expected, tree, io.Discard, maxBytes)
}
