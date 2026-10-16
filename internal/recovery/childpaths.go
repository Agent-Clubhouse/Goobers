package recovery

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
)

func verifySnapshotPolicy(ctx context.Context, repository, snapshot string, policy SnapshotPolicy) error {
	entries, err := readSnapshotTree(ctx, repository, snapshot)
	if err != nil {
		return err
	}
	for len(entries) > 0 {
		entry, rest, complete := bytes.Cut(entries, []byte{0})
		if !complete {
			return fmt.Errorf("invalid child snapshot tree")
		}
		entries = rest
		metadata, name, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return fmt.Errorf("invalid child snapshot tree entry")
		}
		if policy.excludes(string(name)) {
			return fmt.Errorf("child snapshot contains an excluded path")
		}
		fields := strings.Fields(string(metadata))
		if len(fields) != 3 {
			return fmt.Errorf("invalid child snapshot tree metadata")
		}
		switch fields[0] {
		case "160000":
			return ErrNestedRecoveryRequired
		case "120000":
			if err := verifyChildSymlink(ctx, repository, fields[2], string(name), policy); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyChildSymlink(ctx context.Context, repository, object, name string, policy SnapshotPolicy) error {
	var target snapshotPathOutput
	if err := recoveryGit(ctx, repository, &target, "cat-file", "blob", object); err != nil {
		return err
	}
	link := target.String()
	if link == "" || strings.ContainsAny(link, "\\\x00\r\n:") || path.IsAbs(link) {
		return fmt.Errorf("child snapshot symlink escapes permitted workspace")
	}
	resolved := path.Clean(path.Join(path.Dir(name), link))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || policy.excludes(resolved) {
		return fmt.Errorf("child snapshot symlink escapes permitted workspace")
	}
	return nil
}
