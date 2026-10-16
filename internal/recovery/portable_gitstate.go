package recovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
)

// WritePortableGitState captures either HEAD or the real index independently
// of working files. Like the ordinary portable tree, it exports no original
// commit ancestry. The input snapshot supplies the stable capture identity and
// exclusion policy; none of the source's Git state is changed.
func WritePortableGitState(ctx context.Context, repository string, snapshot ChildSnapshot, staged bool, destination io.Writer, maxBytes int64) (PortableSnapshot, error) {
	if err := snapshot.validate(); err != nil {
		return PortableSnapshot{}, err
	}
	if err := CheckChildSnapshotCurrent(ctx, repository, snapshot); err != nil {
		return PortableSnapshot{}, err
	}
	directory, err := privateGitDirectory(ctx, repository, "goobers-parent-state-*")
	if err != nil {
		return PortableSnapshot{}, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	env, err := snapshotEnvironment(ctx, repository, directory, snapshot.Record.BaseSHA)
	if err != nil {
		return PortableSnapshot{}, err
	}
	if staged {
		err = snapshotIndexWithPolicy(ctx, repository, env, &snapshot.Policy)
	} else {
		err = readFilteredRevisionIndex(ctx, repository, env, snapshot.Record.BaseSHA, snapshot.Policy)
	}
	if err != nil {
		return PortableSnapshot{}, err
	}
	tree, err := privateIndexTree(ctx, repository, env)
	if err != nil {
		return PortableSnapshot{}, err
	}
	// Changes during capture are not a coherent HEAD/index/worktree tuple.
	if err := CheckChildSnapshotCurrent(ctx, repository, snapshot); err != nil {
		return PortableSnapshot{}, err
	}
	return writePortableTree(ctx, repository, snapshot, tree, destination, maxBytes)
}

func writePortableTree(ctx context.Context, repository string, state ChildSnapshot, tree string, destination io.Writer, maxBytes int64) (PortableSnapshot, error) {
	state.TreeSHA = tree
	var err error
	state.Record.SnapshotSHA, err = portableRoot(ctx, repository, state)
	if err != nil {
		return PortableSnapshot{}, err
	}
	state.Record.Ref, err = RefForSnapshot(state.Record.RunID, state.Record.SnapshotSHA)
	if err != nil {
		return PortableSnapshot{}, err
	}
	state.Record.PatchDigest, err = WriteSnapshotPatch(ctx, repository, state.Record.BaseSHA, state.Record.SnapshotSHA, io.Discard)
	if err != nil {
		return PortableSnapshot{}, err
	}
	return WritePortableSnapshot(ctx, repository, state, destination, maxBytes)
}

func readFilteredRevisionIndex(ctx context.Context, repository string, env []string, revision string, policy SnapshotPolicy) error {
	if err := recoveryGitWithEnv(ctx, repository, io.Discard, env, "read-tree", revision); err != nil {
		return err
	}
	var entries bytes.Buffer
	bounded := &archiveBudgetWriter{destination: &entries, remaining: maxSnapshotIndexBytes}
	if err := recoveryGitWithEnv(ctx, repository, bounded, env, "ls-files", "--stage", "-z"); err != nil {
		return err
	}
	filtered, err := filterSnapshotIndex(entries.Bytes(), &policy)
	if err != nil {
		return err
	}
	if err := recoveryGitWithEnv(ctx, repository, io.Discard, env, "read-tree", "--empty"); err != nil {
		return err
	}
	return recoveryGitIO(ctx, repository, io.Discard, bytes.NewReader(filtered), env, "update-index", "-z", "--index-info")
}

func privateIndexTree(ctx context.Context, repository string, env []string) (string, error) {
	var output boundedRefOutput
	if err := recoveryGitWithEnv(ctx, repository, &output, env, "write-tree"); err != nil {
		return "", err
	}
	tree := string(bytes.TrimSpace(output.Bytes()))
	if !gitObjectID.MatchString(tree) {
		return "", fmt.Errorf("invalid portable Git state tree")
	}
	return tree, requireSelfContainedSnapshot(ctx, repository, tree)
}

// MaterializePortableGitState restores an imported tuple inside a newly
// initialized private worker checkout. HEAD stays at the synthetic head root;
// the index and working files take their independent captured states. This is
// never an operation for importing worker output into a host checkout.
func MaterializePortableGitState(ctx context.Context, repository string, head, index, working PortableSnapshot) error {
	for _, state := range []PortableSnapshot{head, index, working} {
		if err := state.Validate(); err != nil {
			return err
		}
		if state.Record.RunID != head.Record.RunID || state.Record.RepositoryKey != head.Record.RepositoryKey || !state.Record.CreatedAt.Equal(head.Record.CreatedAt) || !slices.Equal(state.Policy.ExcludedPaths, head.Policy.ExcludedPaths) {
			return fmt.Errorf("portable Git state source differs")
		}
		if err := verifyPortableObjects(ctx, repository, state); err != nil {
			return err
		}
	}
	current, err := snapshotObject(ctx, repository, "HEAD^{commit}")
	if err != nil {
		return err
	}
	if current != head.Record.SnapshotSHA {
		return fmt.Errorf("portable Git state requires its initialized head root")
	}
	if err := portableWorkspaceAttributes(ctx, repository); err != nil {
		return err
	}
	if err := recoveryGit(ctx, repository, io.Discard, "read-tree", "--reset", "-u", working.Record.SnapshotSHA); err != nil {
		return err
	}
	return recoveryGit(ctx, repository, io.Discard, "read-tree", index.Record.SnapshotSHA)
}
