package mcpio

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// WalkFileCallback is the callback signature used by WalkFiles.
// It is called for each regular file encountered during the walk.
// Returning an error stops the walk and returns that error.
type WalkFileCallback func(path string, entry fs.DirEntry) error

// WalkFilesOptions controls the behavior of WalkFiles.
type WalkFilesOptions struct {
	// SkipHiddenDirs, when true, skips directories starting with "."
	// (except the root itself). Default: true.
	SkipHiddenDirs bool
	// SkipHiddenFiles, when true, skips regular files starting with "."
	// (except the root itself). Default: false to preserve filepath.WalkDir's
	// default behavior and avoid silently discarding dotfiles in user dirs.
	SkipHiddenFiles bool
	// FollowSymlinks, when false (default), does not follow symlinks.
	// filepath.WalkDir never follows symlinks, and this helper preserves that.
	FollowSymlinks bool
	// SkipSymlinkEntries, when true, skips entries that are symlinks
	// (at the entry type level). Default: true.
	SkipSymlinkEntries bool
	// SkipDirs, when true, skips directory entries themselves (only files
	// are visited). Default: true.
	SkipDirs bool
	// SkipDirPredicate is an optional custom function to determine if a
	// directory should be skipped. It is called for each directory entry.
	// If it returns true, the directory is skipped (not recursed into).
	// This is called after SkipHiddenDirs checks.
	SkipDirPredicate func(path string, entry fs.DirEntry) bool
}

// DefaultWalkFilesOptions returns a WalkFilesOptions with safe defaults:
// - SkipHiddenDirs: true
// - SkipHiddenFiles: false
// - FollowSymlinks: false
// - SkipSymlinkEntries: true
// - SkipDirs: true
// - SkipDirPredicate: nil
func DefaultWalkFilesOptions() WalkFilesOptions {
	return WalkFilesOptions{
		SkipHiddenDirs:     true,
		SkipHiddenFiles:    false,
		FollowSymlinks:     false,
		SkipSymlinkEntries: true,
		SkipDirs:           true,
		SkipDirPredicate:   nil,
	}
}

// WalkFiles walks the directory tree rooted at root, calling callback for each
// regular file (or matching entry type). It uses filepath.WalkDir internally,
// which never follows symlinks at the OS level, and applies additional filtering
// based on options.
//
// The callback receives the full path and the DirEntry. If callback returns an
// error, the walk stops and returns that error. The root itself is not visited
// as an entry (only its contents are walked).
//
// WalkFiles is useful for consolidating repeated directory scanning logic,
// especially in contexts where symlink safety and hidden-directory handling
// are important (config loading, artifact collection, etc.).
func WalkFiles(root string, callback WalkFileCallback, opts WalkFilesOptions) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}

		if path != root {
			if opts.SkipHiddenDirs && entry.IsDir() && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			if opts.SkipDirPredicate != nil && entry.IsDir() && opts.SkipDirPredicate(path, entry) {
				return filepath.SkipDir
			}
			if opts.SkipHiddenFiles && !entry.IsDir() && strings.HasPrefix(entry.Name(), ".") {
				return nil
			}
		}

		if opts.SkipDirs && entry.IsDir() {
			return nil
		}

		if opts.SkipSymlinkEntries && entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if !entry.Type().IsRegular() && !(opts.SkipSymlinkEntries == false && entry.Type()&fs.ModeSymlink != 0) {
			return nil
		}

		if err := callback(path, entry); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		return nil
	})
}

// WalkYAMLFiles walks the directory tree rooted at root, calling callback for
// each YAML file (.yaml or .yml extension, case-insensitive).
// It uses WalkFiles with sensible defaults for config parsing.
func WalkYAMLFiles(root string, callback WalkFileCallback) error {
	return WalkFiles(root, func(path string, entry fs.DirEntry) error {
		// Filter for YAML extensions only
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		return callback(path, entry)
	}, DefaultWalkFilesOptions())
}

// SumFileSizes walks the directory tree rooted at root and returns the total
// size of all files encountered (respecting the options for what counts as a
// file to include). It never follows symlinks.
func SumFileSizes(root string, opts WalkFilesOptions) (int64, error) {
	var total int64
	err := WalkFiles(root, func(path string, entry fs.DirEntry) error {
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		total += info.Size()
		return nil
	}, opts)
	if err != nil {
		return 0, fmt.Errorf("walk %s: %w", root, err)
	}
	return total, nil
}

// SumAllFileSizes walks the directory tree rooted at root and returns the total
// size of all entries (files, symlinks, etc.) without filtering. It never follows
// symlinks and includes everything (useful for disk usage calculations).
func SumAllFileSizes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk %s: %w", root, err)
	}
	return total, nil
}

// CollectFiles walks the directory tree rooted at root and returns all paths
// and their corresponding DirEntry objects that match the callback predicate.
// callback should return true to include the file, false to skip it.
func CollectFiles(root string, callback func(path string, entry fs.DirEntry) bool, opts WalkFilesOptions) ([]string, error) {
	var paths []string
	err := WalkFiles(root, func(path string, entry fs.DirEntry) error {
		if callback(path, entry) {
			paths = append(paths, path)
		}
		return nil
	}, opts)
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return paths, nil
}
