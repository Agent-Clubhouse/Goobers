// Package safepath resolves a path relative to a trusted root without ever
// following a symlink planted inside that root — the discipline every
// pre-sandbox write into a stage workspace needs (#2413).
//
// internal/sandbox exists because workspace/repo content is untrusted, but
// it only confines the SPAWNED harness subprocess. The harness's own process
// writes into the workspace before that boundary exists (materialized
// context artifacts, MCP runtime config, sandbox runtime directories), and a
// plain os.MkdirAll+os.WriteFile at a predictable .goobers/... path follows a
// repository-controlled symlink at both existing intermediate components and
// the leaf — redirecting a trusted write to an arbitrary host path. Callers
// in that position resolve through this package instead.
//
// Everything here is built on the Lstat-walk/EvalSymlinks discipline already
// used elsewhere in this repo (apiv1.ResolveContainedPath, internal/sandbox);
// it deliberately introduces no O_NOFOLLOW/openat-style primitives.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve joins rel onto root and verifies the result cannot escape it,
// lexically or via a symlinked ancestor — the same containment discipline
// apiv1.ResolveContainedPath applies, adapted to not require the leaf to
// already exist (a config or artifact write creates a new file; EvalSymlinks
// on the full path would reject that).
//
// A naive "EvalSymlinks the immediate parent, ignore the error if it doesn't
// exist yet" check is exploitable: for a path like "link/new/out.md" where
// link points outside root and "new" doesn't exist yet, EvalSymlinks(dir)
// fails closed-looking but open — the error was silently ignored in an
// earlier version of this code — and a subsequent os.MkdirAll would then
// walk through "link" (MkdirAll follows symlinks at existing intermediate
// components, same as any normal path resolution) and create "new" outside
// root. Instead: walk up from the leaf's directory to the nearest ancestor
// that actually exists, EvalSymlinks *that* (it's guaranteed to exist, so
// this can't silently no-op), recheck containment on the result, and only
// then create the missing intermediate components — one at a time with
// os.Mkdir, never os.MkdirAll, so nothing created here can itself be, or
// traverse, a symlink; os.Mkdir fails closed (EEXIST) rather than following
// anything planted at that path between the walk-up and the create.
//
// Missing intermediates are created 0o755; the leaf is left for the caller
// to create with whatever mode it needs (see MkdirLeaf for a directory
// leaf). The returned path is rooted at the canonicalized root, so it is
// safe to hand to os.WriteFile/os.ReadFile as-is.
func Resolve(root, rel string, createMissingDirs bool) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	full := filepath.Join(root, rel)
	relBack, err := filepath.Rel(root, full)
	if err != nil || relBack == ".." || strings.HasPrefix(relBack, ".."+string(filepath.Separator)) || filepath.IsAbs(relBack) {
		return "", fmt.Errorf("path %q escapes the root", rel)
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
	// resolved-and-rechecked — no legitimate caller ever needs to write or
	// read through a pre-existing symlink at the leaf position.
	if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path %q is a symlink; refusing to read or write through it", rel)
	}
	return full, nil
}

// MkdirLeaf is Resolve for a directory target: it resolves rel under root
// with the same no-follow discipline, then creates the leaf itself with
// perm, and returns the resolved path. It is the symlink-safe replacement
// for os.MkdirAll(filepath.Join(root, rel), perm) at a pre-sandbox call site.
//
// An existing directory at the leaf is accepted (matching the os.MkdirAll
// idempotence callers rely on across attempts); an existing non-directory is
// not — and an existing symlink never gets this far, Resolve rejects it.
func MkdirLeaf(root, rel string, perm os.FileMode) (string, error) {
	full, err := Resolve(root, rel, true)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(full, perm); err != nil {
		if !os.IsExist(err) {
			return "", err
		}
		info, statErr := os.Lstat(full)
		if statErr != nil {
			return "", statErr
		}
		if !info.IsDir() {
			return "", fmt.Errorf("path %q exists and is not a directory", rel)
		}
	}
	return full, nil
}
