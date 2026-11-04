package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func removeWindowsReservedCleanupPaths(ctx context.Context, repository string) error {
	// Gone or repository-less targets were already settled by the cleanup guard;
	// there is no checkout whose untracked reserved paths could block removal.
	if _, err := os.Lstat(filepath.Join(repository, ".git")); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect cleanup repository: %w", err)
	}
	selected, err := runCleanupGitOutput(ctx, repository, "list untracked cleanup paths", "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	absoluteRepository, err := filepath.Abs(repository)
	if err != nil {
		return fmt.Errorf("resolve cleanup repository: %w", err)
	}
	for _, path := range strings.Split(selected, "\x00") {
		if path == "" || filepath.IsLocal(path) || !lexicallyLocalCleanupPath(path) {
			continue
		}
		absolute := absoluteRepository + string(filepath.Separator) + filepath.FromSlash(path)
		extended := `\\?\` + absolute
		if strings.HasPrefix(absolute, `\\`) {
			extended = `\\?\UNC\` + strings.TrimPrefix(absolute, `\\`)
		}
		if err := os.Remove(extended); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove reserved cleanup path %q: %w", path, err)
		}
	}
	return nil
}

func lexicallyLocalCleanupPath(path string) bool {
	if path == "" || filepath.VolumeName(path) != "" || strings.Contains(path, ":") ||
		strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
