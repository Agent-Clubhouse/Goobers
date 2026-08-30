package mcpio

import (
	"github.com/goobers/goobers/internal/pathutil"
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
	return pathutil.ResolveRootedPath(root, rel, createMissingDirs)
}
