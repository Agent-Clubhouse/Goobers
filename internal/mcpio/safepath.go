package mcpio

import (
	"github.com/goobers/goobers/internal/pathutil"
)

// resolveRooted resolves rel under root without following a symlink planted
// anywhere in the chain. The logic lives in internal/pathutil, which shares
// this discipline with api/v1alpha1 and internal/configboundary; see
// pathutil.ResolveRootedPath for the rationale.
func resolveRooted(root, rel string, createMissingDirs bool) (string, error) {
	return pathutil.ResolveRootedPath(root, rel, createMissingDirs)
}
