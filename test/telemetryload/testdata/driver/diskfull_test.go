package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskFullRefusesUnmarkedOrOrdinaryVolume(t *testing.T) {
	for _, path := range []string{"relative", filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		if err := validateDiskFullVolume(path); err == nil {
			t.Fatalf("unsafe volume accepted: %s", path)
		}
	}
}

func TestDiskFullRestoreOnlyRemovesOwnedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &diskFullFixture{path: path, identity: identity}
	if err = os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = fixture.restore(); err == nil {
		t.Fatal("removed another file at the fixture path")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "replacement" {
		t.Fatalf("replacement damaged: %q %v", got, err)
	}
	fixture.path = path + "-original"
	if err = fixture.restore(); err != nil {
		t.Fatal(err)
	}
	if err = fixture.restore(); err != nil {
		t.Fatal("restoration is not idempotent")
	}
}
