package recovery

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// snapshotTrackedUpdates selects the tracked paths whose raw worktree bytes
// replace their imported index entries in a capture (#6917).
//
// Capture deliberately disables content conversions, so re-adding every
// tracked file would turn a Git-clean checkout representation (for example an
// LF blob checked out as CRLF by `eol=crlf`) into a different blob: unrelated,
// unchanged files would appear as source modifications. A path is clean only
// when the source repository, under its own attributes and configuration,
// finds the worktree identical to the index; such a path keeps its canonical
// index blob. Everything else is still captured raw:
//   - paths Git reports as modified, deleted, type-changed or unmerged;
//   - assume-unchanged paths, whose edits Git status would not report; and
//   - paths with a filter driver, whose index blob may be an external-store
//     pointer (LFS) rather than the content a recovery bundle must contain.
//
// Status runs with those filter drivers disabled so capture never executes
// external clean filters or fails on a missing required one; the filtered
// paths are captured raw regardless of their status.
func snapshotTrackedUpdates(ctx context.Context, repository, directory string) (string, error) {
	tracked, updates, err := snapshotTrackedEntries(ctx, repository)
	if err != nil {
		return "", err
	}
	drivers, err := snapshotFilteredPaths(ctx, repository, tracked, updates)
	if err != nil {
		return "", err
	}
	if err := snapshotModifiedPaths(ctx, repository, drivers, updates); err != nil {
		return "", err
	}
	paths := make([]string, 0, len(updates))
	for path := range updates {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	var selected bytes.Buffer
	for _, path := range paths {
		selected.WriteString(path)
		selected.WriteByte(0)
	}
	path := filepath.Join(directory, "update-paths")
	if err := os.WriteFile(path, selected.Bytes(), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// snapshotTrackedEntries lists tracked paths present in the sparse checkout
// and marks assume-unchanged ones (lowercase `ls-files -v` tags) for raw
// capture. Skip-worktree entries are excluded: `add --update` never reads them.
func snapshotTrackedEntries(ctx context.Context, repository string) ([]byte, map[string]bool, error) {
	var listing bytes.Buffer
	writer := &snapshotPathBudgetWriter{destination: &listing, remaining: maxSnapshotPathBytes}
	if err := recoveryGit(ctx, repository, writer, "ls-files", "-v", "-z"); err != nil {
		return nil, nil, fmt.Errorf("read recovery tracked paths: %w", err)
	}
	return parseSnapshotTrackedEntries(listing.Bytes())
}

func parseSnapshotTrackedEntries(listing []byte) ([]byte, map[string]bool, error) {
	var tracked bytes.Buffer
	updates := make(map[string]bool)
	for data := listing; len(data) > 0; {
		entry, rest, complete := bytes.Cut(data, []byte{0})
		if !complete || len(entry) < 3 || entry[1] != ' ' {
			return nil, nil, fmt.Errorf("invalid recovery tracked path listing")
		}
		data = rest
		tag, name := entry[0], string(entry[2:])
		if tag == 'S' || tag == 's' {
			continue
		}
		if tag >= 'a' && tag <= 'z' {
			updates[name] = true
		}
		tracked.WriteString(name)
		tracked.WriteByte(0)
	}
	return tracked.Bytes(), updates, nil
}

// snapshotFilteredPaths marks tracked paths that have a filter driver and
// returns the distinct driver names.
func snapshotFilteredPaths(ctx context.Context, repository string, tracked []byte, updates map[string]bool) ([]string, error) {
	if len(tracked) == 0 {
		return nil, nil
	}
	var output bytes.Buffer
	writer := &snapshotPathBudgetWriter{destination: &output, remaining: 2 * maxSnapshotPathBytes}
	if err := recoveryGitIO(ctx, repository, writer, bytes.NewReader(tracked), nil, "check-attr", "-z", "--stdin", "filter"); err != nil {
		return nil, fmt.Errorf("read recovery filter attributes: %w", err)
	}
	return parseSnapshotFilterAttributes(output.Bytes(), updates)
}

// parseSnapshotFilterAttributes reads `check-attr -z filter` output, marking
// paths with a filter driver and returning the distinct driver names.
func parseSnapshotFilterAttributes(output []byte, updates map[string]bool) ([]string, error) {
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, fmt.Errorf("invalid recovery filter attribute listing")
	}
	fields := bytes.Split(output[:len(output)-1], []byte{0})
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("invalid recovery filter attribute listing")
	}
	seen := make(map[string]bool)
	var drivers []string
	for index := 0; index < len(fields); index += 3 {
		path, value := string(fields[index]), string(fields[index+2])
		switch value {
		case "unspecified", "unset", "set":
			continue
		}
		updates[path] = true
		if !seen[value] {
			seen[value] = true
			drivers = append(drivers, value)
		}
	}
	return drivers, nil
}

// snapshotModifiedPaths marks paths whose worktree differs from the index
// under the source repository's own attributes and configuration. Optional
// locks are disabled so status never rewrites the caller's index.
func snapshotModifiedPaths(ctx context.Context, repository string, drivers []string, updates map[string]bool) error {
	var status bytes.Buffer
	writer := &snapshotPathBudgetWriter{destination: &status, remaining: maxSnapshotPathBytes}
	if err := recoveryGitWithEnv(ctx, repository, writer, snapshotStatusEnvironment(drivers), "--no-optional-locks", "status",
		"--porcelain=v1", "-z", "--untracked-files=no", "--no-renames", "--ignore-submodules=none"); err != nil {
		return fmt.Errorf("read recovery tracked changes: %w", err)
	}
	return parseSnapshotWorktreeChanges(status.Bytes(), updates)
}

// snapshotStatusEnvironment makes status compare every stat-dirty file by
// content, without executing the named filter drivers.
func snapshotStatusEnvironment(drivers []string) []string {
	config := [][2]string{{"core.fsmonitor", "false"}, {"core.ignoreStat", "false"}}
	for _, driver := range drivers {
		prefix := "filter." + driver + "."
		config = append(config, [2]string{prefix + "clean", ""}, [2]string{prefix + "process", ""}, [2]string{prefix + "required", "false"})
	}
	environment := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(config))}
	for index, entry := range config {
		suffix := strconv.Itoa(index)
		environment = append(environment, "GIT_CONFIG_KEY_"+suffix+"="+entry[0], "GIT_CONFIG_VALUE_"+suffix+"="+entry[1])
	}
	return environment
}

// parseSnapshotWorktreeChanges marks every path whose `status --porcelain=v1
// -z` worktree column reports a change (modified, deleted, type change,
// intent-to-add or unmerged).
func parseSnapshotWorktreeChanges(status []byte, updates map[string]bool) error {
	for data := status; len(data) > 0; {
		entry, rest, complete := bytes.Cut(data, []byte{0})
		if !complete || len(entry) < 4 || entry[2] != ' ' {
			return fmt.Errorf("invalid recovery tracked change listing")
		}
		data = rest
		if entry[1] != ' ' {
			updates[string(entry[3:])] = true
		}
	}
	return nil
}
