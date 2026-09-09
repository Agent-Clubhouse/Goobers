package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// ErrNestedRecoveryRequired prevents a parent-only bundle from being mistaken
// for recovery of nested repositories. The coordinator must recursively retain
// nested state before it can authorize removal of the containing worktree.
var ErrNestedRecoveryRequired = errors.New("recovery snapshot requires nested repository archives")

func requireSelfContainedSnapshot(ctx context.Context, repository, tree string) error {
	var entries bytes.Buffer
	writer := &archiveBudgetWriter{destination: &entries, remaining: maxSnapshotIndexBytes}
	if err := recoveryGit(ctx, repository, writer, "ls-tree", "-r", "-z", tree); err != nil {
		return fmt.Errorf("inspect recovery snapshot tree: %w", err)
	}
	for data := entries.Bytes(); len(data) > 0; {
		entry, rest, complete := bytes.Cut(data, []byte{0})
		if !complete {
			return fmt.Errorf("invalid recovery snapshot tree listing")
		}
		data = rest
		if bytes.HasPrefix(entry, []byte("160000 ")) {
			return ErrNestedRecoveryRequired
		}
	}
	return nil
}
