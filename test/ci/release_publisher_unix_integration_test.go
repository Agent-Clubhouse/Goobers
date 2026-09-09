//go:build integration && !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationReleasePublisherRejectsSymlinkAssets(t *testing.T) {
	testdep.Require(t, "python3")
	script := publisherVerificationPython(t)
	for _, name := range []string{"dist/install.sh", "dist/SHA256SUMS", "release-notes/RELEASE_NOTES.md"} {
		t.Run(name, func(t *testing.T) {
			root, _ := publisherFixture(t, "v0.4.0-rc.1")
			original := filepath.Join(root, filepath.FromSlash(name))
			// Keep the replacement outside either allowlisted artifact directory, so
			// rejection proves symlink handling rather than extra-file detection.
			replacement := filepath.Join(root, "outside")
			if err := os.Rename(original, replacement); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(replacement, original); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("python3", "-I", "-")
			command.Dir, command.Stdin = root, strings.NewReader(script)
			command.Env = append(os.Environ(), "TAG=v0.4.0-rc.1")
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatalf("linked release content accepted: %s", output)
			}
		})
	}
}
