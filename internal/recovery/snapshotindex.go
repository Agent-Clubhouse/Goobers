package recovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

const maxSnapshotIndexBytes = 32 << 20

// Export logical entries, not raw index bytes: sparse/split index extensions
// can reference shared files belonging to the source Git directory. Importing
// entries preserves staged content without borrowing or rewriting that index.
func snapshotIndex(ctx context.Context, repository string, environment []string) error {
	var entries bytes.Buffer
	writer := &archiveBudgetWriter{destination: &entries, remaining: maxSnapshotIndexBytes}
	if err := recoveryGit(ctx, repository, writer, "ls-files", "--stage", "-z"); err != nil {
		return fmt.Errorf("read recovery source index: %w", err)
	}
	if err := recoveryGitWithEnv(ctx, repository, io.Discard, environment, "read-tree", "--empty"); err != nil {
		return err
	}
	if err := recoveryGitIO(ctx, repository, io.Discard, &entries, environment, "update-index", "-z", "--index-info"); err != nil {
		return fmt.Errorf("import recovery source index: %w", err)
	}
	return snapshotSkipFlags(ctx, repository, environment)
}

func snapshotSkipFlags(ctx context.Context, repository string, environment []string) error {
	var flags bytes.Buffer
	writer := &archiveBudgetWriter{destination: &flags, remaining: maxSnapshotIndexBytes}
	if err := recoveryGit(ctx, repository, writer, "ls-files", "-t", "-z"); err != nil {
		return fmt.Errorf("read recovery sparse flags: %w", err)
	}
	var skipped bytes.Buffer
	for data := flags.Bytes(); len(data) > 0; {
		entry, rest, complete := bytes.Cut(data, []byte{0})
		if !complete || len(entry) < 3 || entry[1] != ' ' {
			return fmt.Errorf("invalid recovery sparse flags")
		}
		data = rest
		if entry[0] == 'S' {
			skipped.Write(entry[2:])
			skipped.WriteByte(0)
		}
	}
	if skipped.Len() == 0 {
		return nil
	}
	if err := recoveryGitIO(ctx, repository, io.Discard, &skipped, environment, "update-index", "--skip-worktree", "-z", "--stdin"); err != nil {
		return fmt.Errorf("preserve recovery sparse flags: %w", err)
	}
	return nil
}
