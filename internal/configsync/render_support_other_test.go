//go:build !linux && !darwin

package configsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWriteManifestsRefusesUnsupportedAtomicPublicationBeforeMutation(t *testing.T) {
	parent := t.TempDir()
	out := filepath.Join(parent, "rendered")
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}

	_, err := set.WriteManifests(out)
	if !errors.Is(err, errAtomicManifestPublicationUnsupported) {
		t.Fatalf("WriteManifests error = %v, want unsupported atomic publication", err)
	}
	if !strings.Contains(err.Error(), "render on Linux or macOS, or use --apply") {
		t.Fatalf("WriteManifests error = %q, want actionable alternative", err)
	}
	if _, statErr := os.Lstat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output created despite refusal: %v", statErr)
	}
	if _, statErr := os.Lstat(ManifestGenerationDir(out)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("generation directory created despite refusal: %v", statErr)
	}
}
