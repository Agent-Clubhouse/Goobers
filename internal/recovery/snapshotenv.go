package recovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A private Git directory gives capture the highest-precedence attributes
// without editing the source's info/attributes. Disable content conversions,
// including LFS/clean filters, encoding, ident and line endings: a recovery
// bundle must contain actual bytes, not external-store pointers.
func snapshotEnvironment(ctx context.Context, repository, directory, parent string) ([]string, error) {
	var objects snapshotPathOutput
	if err := recoveryGit(ctx, repository, &objects, "rev-parse", "--path-format=absolute", "--git-path", "objects"); err != nil {
		return nil, fmt.Errorf("locate recovery object database: %w", err)
	}
	objectPath := strings.TrimSuffix(objects.String(), "\n")
	if !filepath.IsAbs(objectPath) || strings.ContainsAny(objectPath, "\r\n\x00") {
		return nil, fmt.Errorf("invalid recovery object database path")
	}
	worktree, err := filepath.Abs(repository)
	if err != nil {
		return nil, err
	}
	format := "sha1"
	if len(parent) == 64 {
		format = "sha256"
	}
	if err := recoveryGit(ctx, repository, io.Discard, "init", "--bare", "--template=", "--object-format="+format, directory); err != nil {
		return nil, fmt.Errorf("initialize private recovery Git directory: %w", err)
	}
	info := filepath.Join(directory, "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(info, "attributes"), []byte("* -filter -text -ident -working-tree-encoding\n"), 0o600); err != nil {
		return nil, err
	}
	return []string{
		"GIT_DIR=" + directory,
		"GIT_WORK_TREE=" + worktree,
		"GIT_OBJECT_DIRECTORY=" + objectPath,
		"GIT_INDEX_FILE=" + filepath.Join(directory, "index"),
		"GIT_ATTR_NOSYSTEM=1",
	}, nil
}

type snapshotPathOutput struct{ bytes.Buffer }

func (w *snapshotPathOutput) Write(data []byte) (int, error) {
	if w.Len()+len(data) > 8192 {
		return 0, fmt.Errorf("recovery object path exceeds byte budget")
	}
	return w.Buffer.Write(data)
}
