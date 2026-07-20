package gooberassets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func writeAsset(t *testing.T, root, name, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestBundleMaterializationIsIsolated(t *testing.T) {
	sources := []string{t.TempDir(), t.TempDir()}
	writeAsset(t, sources[0], "templates/prompt.txt", "alpha", 0o644)
	writeAsset(t, sources[1], "fixtures/example.json", `{"name":"beta"}`, 0o644)

	bundles := make([]*Bundle, len(sources))
	for i, source := range sources {
		var err error
		bundles[i], err = Load(source)
		if err != nil {
			t.Fatalf("Load(%d): %v", i, err)
		}
	}

	tests := []struct {
		bundle     *Bundle
		present    string
		content    string
		notPresent string
	}{
		{bundles[0], "templates/prompt.txt", "alpha", "fixtures/example.json"},
		{bundles[1], "fixtures/example.json", `{"name":"beta"}`, "templates/prompt.txt"},
	}
	for i, test := range tests {
		workspace := t.TempDir()
		if err := test.bundle.Materialize(workspace); err != nil {
			t.Fatalf("Materialize(%d): %v", i, err)
		}
		data, err := os.ReadFile(filepath.Join(workspace, WorkspaceDir, test.present))
		if err != nil {
			t.Fatalf("read materialized asset %d: %v", i, err)
		}
		if string(data) != test.content {
			t.Fatalf("materialized asset %d = %q, want %q", i, data, test.content)
		}
		if _, err := os.Stat(filepath.Join(workspace, WorkspaceDir, test.notPresent)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("foreign asset leaked into bundle %d: %v", i, err)
		}
	}
}

func TestMissingBundleLeavesWorkspaceUnchanged(t *testing.T) {
	bundle, err := Load(filepath.Join(t.TempDir(), SourceDir))
	if err != nil {
		t.Fatal(err)
	}
	if bundle != nil {
		t.Fatalf("missing source returned bundle %#v", bundle)
	}
	workspace := t.TempDir()
	if err := bundle.Materialize(workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, WorkspaceDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace asset path exists without a bundle: %v", err)
	}

	if err := os.Mkdir(filepath.Join(workspace, WorkspaceDir), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAsset(t, filepath.Join(workspace, WorkspaceDir), "repository-owned.txt", "untouched", 0o644)
	if err := bundle.Materialize(workspace); err != nil {
		t.Fatalf("nil bundle changed a workspace with a repository-owned asset path: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, WorkspaceDir, "repository-owned.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "untouched" {
		t.Fatalf("repository-owned content = %q, want untouched", data)
	}
}

func TestBundleRefusesWorkspaceCollision(t *testing.T) {
	source := t.TempDir()
	writeAsset(t, source, "reference.txt", "asset", 0o644)
	bundle, err := Load(source)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, WorkspaceDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Materialize(workspace); !errors.Is(err, ErrWorkspaceCollision) {
		t.Fatalf("Materialize error = %v, want ErrWorkspaceCollision", err)
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.txt")
	writeAsset(t, filepath.Dir(target), filepath.Base(target), "outside", 0o644)
	if err := os.Symlink(target, filepath.Join(source, "outside.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := Load(source); err == nil {
		t.Fatal("Load accepted a symlink asset")
	}
}

func TestLoadRejectsSymlinkRoot(t *testing.T) {
	source := filepath.Join(t.TempDir(), SourceDir)
	if err := os.Symlink(t.TempDir(), source); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := Load(source); err == nil {
		t.Fatal("Load accepted a symlink asset root")
	}
}

func TestLoadAndValidateRejectSpecialFile(t *testing.T) {
	source := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(source, "stream"), 0o600); err != nil {
		t.Skipf("FIFO unsupported: %v", err)
	}
	if _, err := Load(source); err == nil {
		t.Fatal("Load accepted a FIFO asset")
	}
	if err := Validate(source); err == nil {
		t.Fatal("Validate accepted a FIFO asset")
	}
}

func TestSourceDirectoryDetection(t *testing.T) {
	source := filepath.Join("config", "gaggles", "example", "goobers", "coder", SourceDir)
	if !IsSourceDir(source) {
		t.Fatalf("IsSourceDir(%q) = false", source)
	}
	if !IsWithinSourceDir(filepath.Join(source, ".hidden", "fixture.yaml")) {
		t.Fatal("nested asset was not detected")
	}
	if IsSourceDir(filepath.Join("config", SourceDir)) {
		t.Fatal("unscoped assets directory was detected as goober assets")
	}
}
