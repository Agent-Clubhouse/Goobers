package mcpio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishOutputManifestTarget(t *testing.T) {
	root := t.TempDir()
	tool := NewToolset(Config{Workspace: root, ArtifactManifestFile: "output/manifest.json"})
	const content = `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[]}`
	n, digest, err := tool.PublishOutput(content)
	if err != nil || n != len(content) || digest == "" {
		t.Fatalf("publish: %d, %q, %v", n, digest, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "output", "manifest.json"))
	if err != nil || string(data) != content {
		t.Fatalf("manifest: %q, %v", data, err)
	}
	tool = NewToolset(Config{Workspace: root, ArtifactFile: "legacy", ArtifactManifestFile: "manifest"})
	if _, _, err := tool.PublishOutput(content); err == nil {
		t.Fatal("ambiguous output target accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "legacy")); !os.IsNotExist(err) {
		t.Fatalf("ambiguous target wrote legacy file: %v", err)
	}
}
