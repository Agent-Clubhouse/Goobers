package mcpio

import "github.com/goobers/goobers/internal/safepath"

// resolveRooted resolves rel under root without following a symlink planted
// anywhere in the chain. The logic lives in internal/safepath because the
// harness's other pre-sandbox workspace writes need exactly the same
// discipline (#2413); see safepath.Resolve for the rationale.
func resolveRooted(root, rel string, createMissingDirs bool) (string, error) {
	return safepath.Resolve(root, rel, createMissingDirs)
}
