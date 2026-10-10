package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func portableWorkspaceAttributes(ctx context.Context, repository string) error {
	// Portable files are raw captured bytes. Repository attributes must not
	// invoke filters or reinterpret encodings while materializing this tuple.
	var gitDir snapshotPathOutput
	if err := recoveryGit(ctx, repository, &gitDir, "rev-parse", "--path-format=absolute", "--git-dir"); err != nil {
		return err
	}
	gitPath := strings.TrimSuffix(gitDir.String(), "\n")
	if !filepath.IsAbs(gitPath) || strings.ContainsAny(gitPath, "\r\n\x00") {
		return fmt.Errorf("invalid portable Git directory path")
	}
	info := filepath.Join(gitPath, "info")
	if err := os.MkdirAll(info, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(info, "attributes"), []byte("* -filter -text -ident -working-tree-encoding\n"), 0600)
}
