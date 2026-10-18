package recovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

func planChildIndex(ctx context.Context, repository, head string, changes []childFileChange) (string, error) {
	directory, err := os.MkdirTemp("", "goobers-child-index-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	environment, err := snapshotEnvironment(ctx, repository, directory, head)
	if err != nil {
		return "", err
	}
	if err := snapshotIndexWithPolicy(ctx, repository, environment, nil); err != nil {
		return "", err
	}
	if err := copyChildAssumeFlags(ctx, repository, environment); err != nil {
		return "", err
	}
	if err := updateChildIndex(ctx, repository, environment, changes); err != nil {
		return "", err
	}
	return workspaceIndexDigest(ctx, repository, environment)
}

func copyChildAssumeFlags(ctx context.Context, repository string, environment []string) error {
	var flags bytes.Buffer
	writer := &archiveBudgetWriter{destination: &flags, remaining: maxSnapshotIndexBytes}
	if err := recoveryGit(ctx, repository, writer, "ls-files", "-v", "-z"); err != nil {
		return err
	}
	var assumed bytes.Buffer
	for data := flags.Bytes(); len(data) > 0; {
		entry, rest, ok := bytes.Cut(data, []byte{0})
		if !ok || len(entry) < 3 || entry[1] != ' ' {
			return fmt.Errorf("invalid child index flags")
		}
		data = rest
		if entry[0] >= 'a' && entry[0] <= 'z' {
			assumed.Write(entry[2:])
			assumed.WriteByte(0)
		}
	}
	if assumed.Len() == 0 {
		return nil
	}
	return recoveryGitIO(ctx, repository, io.Discard, &assumed, environment, "update-index", "--assume-unchanged", "-z", "--stdin")
}

func updateChildIndex(ctx context.Context, repository string, environment []string, changes []childFileChange) error {
	if len(changes) == 0 {
		return nil
	}
	var entries bytes.Buffer
	for _, change := range changes {
		if change.after.object == "" {
			fmt.Fprintf(&entries, "0 %s\t%s%c", strings.Repeat("0", len(change.before.object)), change.name, 0)
		} else {
			fmt.Fprintf(&entries, "%s %s\t%s%c", change.after.mode, change.after.object, change.name, 0)
		}
	}
	return recoveryGitIO(ctx, repository, io.Discard, &entries, environment, "-c", "core.fsync=index", "update-index", "-z", "--index-info")
}
