package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
)

type childTreeEntry struct{ mode, object string }
type childFileChange struct {
	name                  string
	before, after         childTreeEntry
	beforeData, afterData []byte
}

func childTree(ctx context.Context, repository, tree string) (map[string]childTreeEntry, error) {
	data, err := readSnapshotTree(ctx, repository, tree)
	if err != nil {
		return nil, err
	}
	entries := map[string]childTreeEntry{}
	for len(data) > 0 {
		entry, rest, ok := bytes.Cut(data, []byte{0})
		if !ok {
			return nil, fmt.Errorf("incomplete child tree")
		}
		data = rest
		meta, name, ok := bytes.Cut(entry, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 3 || fields[1] != "blob" || !gitObjectID.MatchString(fields[2]) || !validChildApplyPath(string(name)) {
			return nil, fmt.Errorf("invalid child tree entry")
		}
		if fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000" {
			return nil, fmt.Errorf("unsupported child tree mode")
		}
		entries[string(name)] = childTreeEntry{fields[0], fields[2]}
	}
	return entries, nil
}

func validChildApplyPath(name string) bool {
	return name != "" && len(name) <= 4096 && path.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\\x00\r\n")
}

func loadChildChanges(ctx context.Context, repository string, p PreparedChildDisposition) ([]childFileChange, error) {
	before, err := childTree(ctx, repository, p.ExpectedParent.TreeSHA)
	if err != nil {
		return nil, err
	}
	after, err := childTree(ctx, repository, p.TreeSHA)
	if err != nil {
		return nil, err
	}
	if err := checkChildCaseAliases(before, after); err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for name, entry := range before {
		if after[name] != entry {
			keys[name] = true
		}
	}
	for name, entry := range after {
		if before[name] != entry {
			keys[name] = true
		}
	}
	if len(keys) > maxChildApplyPaths {
		return nil, fmt.Errorf("child application exceeds %d changed paths", maxChildApplyPaths)
	}
	names := make([]string, 0, len(keys))
	for name := range keys {
		names = append(names, name)
	}
	slices.Sort(names)
	budget := int64(maxChildApplyBytes)
	changes := make([]childFileChange, 0, len(names))
	for _, name := range names {
		if p.ExpectedParent.Policy.excludes(name) {
			return nil, fmt.Errorf("child application targets an excluded path")
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if before[parent].object != "" || after[parent].object != "" {
				return nil, fmt.Errorf("child file/directory replacement requires manual resolution")
			}
		}
		change := childFileChange{name: name, before: before[name], after: after[name]}
		change.beforeData, err = childBlob(ctx, repository, change.before, &budget)
		if err != nil {
			return nil, err
		}
		change.afterData, err = childBlob(ctx, repository, change.after, &budget)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func checkChildCaseAliases(trees ...map[string]childTreeEntry) error {
	seen := map[string]string{}
	for _, tree := range trees {
		for name := range tree {
			folded := strings.ToLower(name)
			if prior, ok := seen[folded]; ok && prior != name {
				return fmt.Errorf("case-alias child paths require manual resolution")
			}
			seen[folded] = name
		}
	}
	return nil
}

func childBlob(ctx context.Context, repository string, entry childTreeEntry, budget *int64) ([]byte, error) {
	if entry.object == "" {
		return nil, nil
	}
	var data bytes.Buffer
	writer := &archiveBudgetWriter{destination: &data, remaining: *budget}
	if err := recoveryGit(ctx, repository, writer, "cat-file", "blob", entry.object); err != nil {
		return nil, err
	}
	*budget -= int64(data.Len())
	return data.Bytes(), nil
}

func childAncestors(root *os.Root, name string) error {
	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		info, err := root.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("child target has a non-directory ancestor")
		}
	}
	return nil
}

func childFileValue(root *os.Root, name string) (childTreeEntry, []byte, error) {
	if err := childAncestors(root, name); err != nil {
		return childTreeEntry{}, nil, err
	}
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return childTreeEntry{}, nil, nil
	}
	if err != nil {
		return childTreeEntry{}, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		data, err := root.Readlink(name)
		return childTreeEntry{mode: "120000"}, []byte(data), err
	}
	if !info.Mode().IsRegular() || info.Size() > maxChildApplyBytes {
		return childTreeEntry{}, nil, fmt.Errorf("child target is not a bounded regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return childTreeEntry{}, nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxChildApplyBytes+1))
	if len(data) > maxChildApplyBytes {
		return childTreeEntry{}, nil, fmt.Errorf("child target exceeds byte limit")
	}
	mode := "100644"
	if info.Mode().Perm()&0111 != 0 {
		mode = "100755"
	}
	return childTreeEntry{mode: mode}, data, err
}

func childFileMatches(value childTreeEntry, data []byte, expected childTreeEntry, content []byte) bool {
	return value.mode == expected.mode && bytes.Equal(data, content)
}

func checkChildFiles(root *os.Root, changes []childFileChange, retry bool) error {
	for _, change := range changes {
		value, data, err := childFileValue(root, change.name)
		if err != nil {
			return err
		}
		if childFileMatches(value, data, change.before, change.beforeData) {
			continue
		}
		if retry && childFileMatches(value, data, change.after, change.afterData) {
			continue
		}
		return fmt.Errorf("%w: child application path %q", ErrWorkspaceChanged, change.name)
	}
	return nil
}

func checkUnrelatedChildFiles(ctx context.Context, repository string, p PreparedChildDisposition, changes []childFileChange) error {
	e := p.ExpectedParent
	current, err := CaptureChildSnapshot(ctx, repository, e.Record.RepositoryKey, e.Record.RunID, e.Record.CreatedAt, e.Record.RetainUntil, e.Policy)
	if err != nil {
		return err
	}
	before, err := childTree(ctx, repository, e.TreeSHA)
	if err != nil {
		return err
	}
	live, err := childTree(ctx, repository, current.TreeSHA)
	if err != nil {
		return err
	}
	for _, change := range changes {
		delete(before, change.name)
		delete(live, change.name)
	}
	if len(before) != len(live) {
		return ErrWorkspaceChanged
	}
	for name, entry := range before {
		if live[name] != entry {
			return ErrWorkspaceChanged
		}
	}
	return nil
}

func writeChildFile(root *os.Root, change childFileChange, operation string) error {
	value, data, err := childFileValue(root, change.name)
	if err != nil {
		return err
	}
	if childFileMatches(value, data, change.after, change.afterData) {
		return nil
	}
	if !childFileMatches(value, data, change.before, change.beforeData) {
		return ErrWorkspaceChanged
	}
	if change.after.object == "" {
		if err := root.Remove(change.name); err != nil {
			return err
		}
		return syncChildDirectory(root, path.Dir(change.name))
	}
	if err := root.MkdirAll(path.Dir(change.name), 0700); err != nil {
		return err
	}
	// One reserved runtime staging name bounds crash debris. It is excluded
	// from snapshots and belongs to this durable operation, under exclusive custody.
	staging := ".goobers/child-apply-" + operation
	if err := childAncestors(root, staging); err != nil {
		return err
	}
	if err := root.MkdirAll(".goobers", 0700); err != nil {
		return err
	}
	if err := root.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	defer func() { _ = root.Remove(staging) }()
	if err := stageChildFile(root, staging, change); err != nil {
		return err
	}
	if err := root.Rename(staging, change.name); err != nil {
		return err
	}
	if err := syncChildDirectory(root, path.Dir(change.name)); err != nil {
		return err
	}
	return syncChildDirectory(root, ".goobers")
}

func stageChildFile(root *os.Root, name string, change childFileChange) error {
	if change.after.mode == "120000" {
		return root.Symlink(string(change.afterData), name)
	}
	mode := os.FileMode(0644)
	if change.after.mode == "100755" {
		mode = 0755
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(change.afterData); err != nil {
		return err
	}
	return file.Sync()
}

func syncChildDirectory(root *os.Root, name string) error {
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
