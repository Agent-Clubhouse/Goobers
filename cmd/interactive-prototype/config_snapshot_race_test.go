//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// WalkDir caches sibling entries before invoking callbacks. A source editor can
// replace a later ordinary file after enumeration but before its content read.
func TestConfigSnapshotRejectsFileReplacedBySymlink(t *testing.T) {
	source := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private.md")
	if err := os.WriteFile(outside, []byte("private material outside configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "z.md"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("safe"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	copiedOutside := false
	_, err := walkConfig(source, func(name string, data []byte) error {
		if name == "a.md" {
			victim := filepath.Join(source, "z.md")
			if err := os.Remove(victim); err != nil {
				return err
			}
			return os.Symlink(outside, victim)
		}
		copiedOutside = string(data) == "private material outside configuration"
		return nil
	})
	if err == nil || copiedOutside {
		t.Fatalf("snapshot accepted a replacement symlink: err=%v, copied outside data=%v", err, copiedOutside)
	}
}

func TestConfigSnapshotRejectsSymlinkRoot(t *testing.T) {
	source := t.TempDir()
	link := filepath.Join(t.TempDir(), "config")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, err := walkConfig(link, nil); err == nil {
		t.Fatal("snapshot accepted a symlink source root")
	}
}
