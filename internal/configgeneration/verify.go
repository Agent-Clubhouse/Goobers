package configgeneration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"

	"github.com/goobers/goobers/internal/configtree"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

// VerifyDirectory checks an extracted tree against its original content-addressed
// manifest. Windows cannot reproduce Unix mode bits; the archive identity remains
// unchanged while every representable permission, path, type and byte is checked.
func VerifyDirectory(ctx context.Context, configDir, digest, instanceID string) error {
	absolute, err := filepath.Abs(configDir)
	if err != nil {
		return err
	}
	archive, err := readRetainedArchive(filepath.Dir(absolute), digest)
	if err != nil {
		return err
	}
	if archive.InstanceID != instanceID {
		return errors.New("config generation belongs to a different instance")
	}
	return verifyRetainedTree(ctx, absolute, archive)
}

func verifyRetainedTree(ctx context.Context, configDir string, archive *Archive) error {
	absolute, err := filepath.Abs(configDir)
	if err != nil {
		return err
	}
	remaining := make(map[string]file, len(archive.Files))
	for _, entry := range archive.Files {
		remaining[entry.Path] = entry
	}
	err = configtree.WalkDefinitionTrees(absolute, func(tree string) error {
		prefix := "config"
		if tree != absolute {
			prefix = "goobers"
		}
		return verifyRetainedSubtree(ctx, tree, prefix, remaining)
	})
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return errors.New("retained config generation is missing archived paths")
	}
	return nil
}

func verifyRetainedSubtree(ctx context.Context, tree, prefix string, remaining map[string]file) error {
	info, err := os.Lstat(tree)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("retained config root is not a real directory")
	}
	root, err := os.OpenRoot(tree)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		key := path.Join(prefix, filepath.ToSlash(name))
		expected, ok := remaining[key]
		if !ok {
			return fmt.Errorf("retained config generation contains unexpected path %q", key)
		}
		if err := verifyRetainedEntry(root, name, entry, expected); err != nil {
			return fmt.Errorf("retained config path %q: %w", key, err)
		}
		delete(remaining, key)
		return nil
	})
}

func verifyRetainedEntry(root *os.Root, name string, entry fs.DirEntry, expected file) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	if expected.Directory != info.IsDir() || (!info.IsDir() && !info.Mode().IsRegular()) {
		return errors.New("archive file type changed")
	}
	if !retainedModeMatches(info.Mode(), expected.Mode, expected.Directory) {
		return errors.New("archive permissions changed")
	}
	if expected.Directory {
		return nil
	}
	file, err := safeopen.OpenRegularInRoot(root, name)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !retainedModeMatches(opened.Mode(), expected.Mode, false) {
		return errors.New("archive file changed during verification")
	}
	actual, err := io.ReadAll(io.LimitReader(file, int64(len(expected.Data))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected.Data) {
		return errors.New("archive content changed")
	}
	return nil
}

func retainedModeMatches(actual fs.FileMode, expected uint32, directory bool) bool {
	if runtime.GOOS != "windows" {
		return uint32(actual.Perm()) == expected
	}
	// Go's Windows Chmod only implements owner-write as the read-only file
	// attribute. Directory Unix permissions are not representable at all.
	if directory {
		return true
	}
	return (actual.Perm()&0200 != 0) == (expected&0200 != 0)
}
