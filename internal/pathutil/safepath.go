// Package pathutil provides shared, filesystem-agnostic helpers for
// validating that a relative path stays contained within a root: rejecting
// absolute or volume-bound paths, detecting ".." lexical escapes, and
// resolving symlinks to catch escapes that only appear after the filesystem
// follows a link. These checks back the containment guarantees used by
// api/v1alpha1 (journal paths), the config boundary, and mcpio's rooted-path
// resolution, so the same rules aren't reimplemented per caller.
package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IsRootedOrVolumeBound reports whether path is absolute or volume-bound in any
// of the platform-specific ways (leading /, leading \, absolute per filepath.IsAbs,
// or Windows drive letter like C:).
func IsRootedOrVolumeBound(path string) bool {
	return filepath.IsAbs(path) ||
		filepath.VolumeName(path) != "" ||
		strings.HasPrefix(path, "/") ||
		strings.HasPrefix(path, `\`) ||
		(len(path) >= 2 && path[1] == ':' &&
			((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')))
}

// IsLexicallyContained reports whether rel is lexically contained within root
// (no absolute path, no ".." traversal). When root is empty, the path is
// validated for containment without being made absolute (used by structural
// validation which does not touch the filesystem).
//
// Returns the cleaned path (or cleaned absolute path if root is set), or an error
// with the message "path escapes root: <details>".
func IsLexicallyContained(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("path escapes root: empty path")
	}
	if IsRootedOrVolumeBound(rel) {
		return "", fmt.Errorf("path escapes root: %q is absolute or volume-bound", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root: %q", rel)
	}
	if root == "" {
		return clean, nil
	}
	return filepath.Join(root, clean), nil
}

// ValidateSymlinkEscape reports whether the given resolved path escapes the given
// resolved root. It checks whether rel (the result of filepath.Rel(resolvedRoot, resolved))
// attempts to escape via ".." traversal. Returns nil if contained, or a formatted error
// with message "symlink escape: <details>".
func ValidateSymlinkEscape(resolvedRoot, resolved, origRel string) error {
	relBack, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || relBack == ".." || strings.HasPrefix(relBack, ".."+string(filepath.Separator)) {
		return fmt.Errorf("symlink escape: %q resolves to %q", origRel, resolved)
	}
	return nil
}

// ResolveWithSymlinks resolves symlinks in root and full (the absolute joined path),
// and validates that the resolved full path still stays within the resolved root.
// This is the core symlink containment check used by ResolveContainedPath.
//
// Returns (resolvedFull, nil) if contained, or (empty, error) if it escapes.
func ResolveWithSymlinks(root, full, origRel string) (string, error) {
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	if err := ValidateSymlinkEscape(resolvedRoot, resolved, origRel); err != nil {
		return "", err
	}
	return resolved, nil
}

// ResolveRootedPath is the complex version used by mcpio: it handles non-existent
// intermediate directories, walking up to an existing ancestor before using EvalSymlinks.
// This avoids the problem where a non-existent path segment fails EvalSymlinks silently,
// allowing intermediate symlinks to be followed by later os.MkdirAll calls.
//
// If createMissingDirs is true, missing intermediate directories are created one at a time
// (never with os.MkdirAll, which would follow symlinks). If false, it returns an error for
// any missing intermediate directory.
//
// The leaf file itself is never followed through a symlink — a pre-existing symlink at the
// leaf position is rejected outright.
//
// Returns the final (real) path, or an error. Errors include:
// - "path escapes root: ..." for lexical containment violations
// - "path ... escapes the root" for symlink escapes
// - filesystem errors (permission denied, I/O errors, etc.)
func ResolveRootedPath(root, rel string, createMissingDirs bool) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("path escapes root: empty path")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	full := filepath.Join(root, rel)
	relBack, err := filepath.Rel(root, full)
	if err != nil || relBack == ".." || strings.HasPrefix(relBack, ".."+string(filepath.Separator)) || filepath.IsAbs(relBack) {
		return "", fmt.Errorf("path escapes root: %q", rel)
	}

	// Walk up from the leaf's directory to the nearest ancestor that
	// actually exists — an intermediate component further down may not
	// exist yet, and EvalSymlinks requires its argument to exist.
	existing := filepath.Dir(full)
	var missing []string // deepest-first
	for existing != root {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("path %q: %w", rel, err)
		}
		missing = append(missing, filepath.Base(existing))
		existing = filepath.Dir(existing)
	}

	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", rel, err)
	}
	relExisting, err := filepath.Rel(root, resolvedExisting)
	if err != nil || relExisting == ".." || strings.HasPrefix(relExisting, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q's directory escapes the root", rel)
	}

	dir := resolvedExisting
	if len(missing) > 0 {
		if !createMissingDirs {
			return "", fmt.Errorf("path %q: no such directory", rel)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			dir = filepath.Join(dir, missing[i])
			if err := os.Mkdir(dir, 0o755); err != nil {
				return "", fmt.Errorf("create directory for %q: %w", rel, err)
			}
		}
	}

	full = filepath.Join(dir, filepath.Base(full))
	// The leaf itself may already exist as a symlink to somewhere outside
	// root — lexically "full" is still inside root, so the checks above
	// pass, but os.WriteFile/os.ReadFile follow a symlink at open() time and
	// would write or read through it to wherever it actually points. Lstat
	// (not Stat) so this inspects the link itself, not its target; any
	// existing symlink at this exact path is rejected outright rather than
	// resolved-and-rechecked — no legitimate caller of this package ever
	// needs to write or read through a pre-existing symlink at the leaf
	// position.
	if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path %q is a symlink; refusing to read or write through it", rel)
	}
	return full, nil
}
