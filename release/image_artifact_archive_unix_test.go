//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImageArtifactRejectsSymlinkFilesAndParents(t *testing.T) {
	target := Target{"linux", "amd64"}
	data := finalArchiveBytes(t, target, []archiveEntry{{name: "goobers", mode: 0755, data: []byte("binary")}})
	for _, name := range []string{"SHA256SUMS", target.archiveName("v0.4.0-rc.1")} {
		directory := finalArchiveFixture(t, target, data)
		original := filepath.Join(directory, name)
		if err := os.Rename(original, original+".real"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(original+".real", original); err != nil {
			t.Fatal(err)
		}
		if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target); err == nil {
			t.Fatalf("symlink artifact %s accepted", name)
		}
	}
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "real", "input"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(directory, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularImageArtifact(directory, "linked/input", 1024); err == nil {
		t.Fatal("linked artifact parent accepted")
	}
}
