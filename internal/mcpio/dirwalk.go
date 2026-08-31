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
	// IgnoreNotExist, when true, continues when filepath.WalkDir reports an
	// fs.ErrNotExist error. It does not apply to errors returned by callback.
	// Default: false.
	IgnoreNotExist bool
}

// DefaultWalkFilesOptions returns a WalkFilesOptions with safe defaults:
// - SkipHiddenDirs: true
// - SkipHiddenFiles: false
// - SkipSymlinkEntries: true
// - SkipDirs: true
// - SkipDirPredicate: nil
// - IgnoreNotExist: false
func DefaultWalkFilesOptions() WalkFilesOptions {
	return WalkFilesOptions{
		SkipHiddenDirs:     true,
		SkipHiddenFiles:    false,
		SkipSymlinkEntries: true,
		SkipDirs:           true,
		SkipDirPredicate:   nil,
		IgnoreNotExist:     false,
	}
}

// WalkFiles walks the directory tree rooted at root, calling callback for each
// regular file (or matching entry type). It uses filepath.WalkDir internally,
// which never follows symlinks at the OS level, and applies additional filtering
// based on options.
//
// The callback receives the full path and the DirEntry. If callback returns an
// error, the walk stops and returns that error. The root itself is not visited
// as an entry (only its contents are walked). When IgnoreNotExist is true,
// fs.ErrNotExist errors reported by the underlying walk are ignored.
//
// WalkFiles is useful for consolidating repeated directory scanning logic,
// especially in contexts where symlink safety and hidden-directory handling
// are important (config loading, artifact collection, etc.).
func WalkFiles(root string, callback WalkFileCallback, opts WalkFilesOptions) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if opts.IgnoreNotExist && errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}

		if path != root {
			if opts.SkipHiddenDirs && entry.IsDir() && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			if opts.SkipHiddenFiles && !entry.IsDir() && strings.HasPrefix(entry.Name(), ".") {
				return nil
			}
		}
		if opts.SkipDirPredicate != nil && entry.IsDir() && opts.SkipDirPredicate(path, entry) {
			return filepath.SkipDir
		}

		if opts.SkipDirs && entry.IsDir() {
			return nil
		}

		if !entry.Type().IsRegular() &&
			(opts.SkipSymlinkEntries || entry.Type()&fs.ModeSymlink == 0) {
			return nil
		}

		return callback(path, entry)
	})
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
