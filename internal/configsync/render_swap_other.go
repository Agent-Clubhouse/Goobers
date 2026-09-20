//go:build !linux && !darwin

package configsync

func swapManifestPaths(_, _ string) error {
	return validateManifestPublicationSupport()
}
