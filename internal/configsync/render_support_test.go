package configsync

import "testing"

func requireManifestPublication(t *testing.T) {
	t.Helper()
	if err := validateManifestPublicationSupport(); err != nil {
		t.Skip(err)
	}
}
