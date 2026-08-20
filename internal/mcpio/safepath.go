package mcpio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveRooted joins rel onto root and verifies the result cannot escape
// it, lexically or via a symlinked ancestor — the same containment
// discipline internal/harness's InputArtifactFile lift and internal/sandbox
// use elsewhere, adapted to not require the leaf to already exist (a
// publish_output or config write creates a new file; ResolveContainedPath's
// EvalSymlinks on the full path would reject that).
//
// A naive "EvalSymlinks the immediate parent, ignore the error if it
// doesn't exist yet" check is exploitable: for a path like "link/new/out.md"
// where link points outside root and "new" doesn't exist yet,
// EvalSymlinks(dir) fails closed-looking but open — the error was silently
// ignored in an earlier version of this code — and a subsequent
// os.MkdirAll would then walk through "link" (MkdirAll follows symlinks at
// existing intermediate components, same as any normal path resolution)
// and create "new" outside root. Instead: walk up from the leaf's directory
// to the nearest ancestor that actually exists, EvalSymlinks *that* (it's
// guaranteed to exist, so this can't silently no-op), recheck containment
// on the result, and only then create the missing intermediate components —
// one at a time with os.Mkdir, never os.MkdirAll, so nothing created here
// can itself be, or traverse, a symlink; os.Mkdir fails closed (EEXIST)
// rather than following anything planted at that path between the walk-up
// and the create. This matters beyond the read/write tools: any harness
// code writing into a workspace it doesn't fully trust (repository content
// can plant a symlink at a predictable path) needs the same discipline —
// see #2413 for the other call sites that still do the naive thing.
func resolveRooted(root, rel string, createMissingDirs bool) (string, error) {
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
	// resolved-and-rechecked — no legitimate caller of this package ever
	// needs to write or read through a pre-existing symlink at the leaf
	// position.
	if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path %q is a symlink; refusing to read or write through it", rel)
	}
	return full, nil
}
