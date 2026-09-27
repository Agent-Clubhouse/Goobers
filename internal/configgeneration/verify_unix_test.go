//go:build !windows

package configgeneration

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManifestVerificationEnforcesUnixExecutableAndDirectoryModes(t *testing.T) {
	for _, name := range []string{"file execute bit", "directory access bits"} {
		t.Run(name, func(t *testing.T) {
			store, directory, digest := unixGenerationFixture(t)
			target, mode := filepath.Join(directory, "run.sh"), os.FileMode(0644)
			if name == "directory access bits" {
				target, mode = directory, 0700
			}
			if err := os.Chmod(target, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(t.Context(), digest); err == nil {
				t.Fatal("Unix permission mutation accepted")
			}
			if err := VerifyDirectory(t.Context(), directory, digest, "source-instance"); err == nil {
				t.Fatal("CLI accepted Unix permission mutation")
			}
		})
	}
}
