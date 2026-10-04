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
	"strings"

	"github.com/goobers/goobers/internal/pathutil"
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
// safe to hand to os.WriteFile/os.ReadFile as-is. The filesystem checks are
// delegated to pathutil.ResolveRootedPath; this wrapper preserves safepath's
// legacy diagnostics.
func Resolve(root, rel string, createMissingDirs bool) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	full, err := pathutil.ResolveRootedPath(root, rel, createMissingDirs)
	if err != nil && strings.HasPrefix(err.Error(), "path escapes root: ") {
		return "", fmt.Errorf("path %q escapes the root", rel)
	}
	return full, err
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
