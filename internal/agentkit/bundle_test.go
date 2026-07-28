package agentkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	goobersassets "github.com/goobers/goobers"
)

func TestBuildMatchesReleaseToolkitLayout(t *testing.T) {
	bundle := testBundle(t, "v1.2.3", "abc123")
	if bundle.Manifest.BundleVersion != BundleVersion {
		t.Fatalf("bundle version = %q, want %q", bundle.Manifest.BundleVersion, BundleVersion)
	}
	if bundle.Manifest.Producer != (Producer{Version: "v1.2.3", Commit: "abc123"}) {
		t.Fatalf("producer = %+v", bundle.Manifest.Producer)
	}

	goldenPath := filepath.Join("..", "..", "release", "testdata", "agent-toolkit-layout.golden")
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, asset := range bundle.Manifest.Assets {
		got.WriteString(asset.Path)
		got.WriteByte('\n')
		installedPath, err := InstalledAssetPath(asset.Path)
		if err != nil {
			t.Fatal(err)
		}
		file, ok := bundle.Files[installedPath]
		if !ok {
			t.Errorf("manifest asset %q has no bundled file", asset.Path)
			continue
		}
		if int64(len(file.Data)) != asset.Size ||
			digest(file.Data) != asset.SHA256 ||
			formatAssetMode(file.Mode) != asset.Mode {
			t.Errorf("manifest metadata does not match %q", asset.Path)
		}
	}
	if got.String() != string(golden) {
		t.Fatalf("embedded toolkit layout differs from release golden\nwant:\n%s\ngot:\n%s", golden, got.String())
	}
	if _, err := DecodeManifest(bundle.ManifestJSON); err != nil {
		t.Fatalf("decode generated manifest: %v", err)
	}
	const executable = "payload/" + InstalledRoot + "/config-examples/gaggles/acme-web/scripts/check-todos.sh"
	for _, asset := range bundle.Manifest.Assets {
		if asset.Path == executable && asset.Mode != "0755" {
			t.Fatalf("executable asset mode = %q, want 0755", asset.Mode)
		}
	}
}

func TestBundleAdapterMapsGenericConsumer(t *testing.T) {
	bundle := testBundle(t, "dev", "none")
	tests := map[string]string{
		"copilot": ".github/copilot-instructions.md",
		"claude":  "CLAUDE.md",
		"generic": "AGENTS.md",
	}
	for harness, target := range tests {
		adapter, ok := bundle.Adapter(harness)
		if !ok {
			t.Fatalf("adapter %q not found", harness)
		}
		if adapter.InstructionTarget != target {
			t.Errorf("%s instruction target = %q, want %q", harness, adapter.InstructionTarget, target)
		}
	}
	if _, ok := bundle.Adapter("unknown"); ok {
		t.Fatal("unknown harness unexpectedly resolved")
	}
}

func TestBundleDeclaresRepositoryAuthoringCommands(t *testing.T) {
	bundle := testBundle(t, "dev", "none")
	for _, command := range []string{"versions", "features", "examples list", "examples show", "validate"} {
		if !slices.Contains(bundle.Manifest.CLICapabilities.Required, command) {
			t.Errorf("repository authoring command %q is not required", command)
		}
		if slices.Contains(bundle.Manifest.CLICapabilities.Optional, command) {
			t.Errorf("repository authoring command %q is still optional", command)
		}
	}
}

func TestDecodeManifestRejectsUnsafeAssetPath(t *testing.T) {
	bundle := testBundle(t, "dev", "none")
	var raw map[string]any
	if err := json.Unmarshal(bundle.ManifestJSON, &raw); err != nil {
		t.Fatal(err)
	}
	assets := raw["assets"].([]any)
	assets[0].(map[string]any)["path"] = "payload/.goobers/agent-toolkit/../escape"
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeManifest(data); err == nil {
		t.Fatal("unsafe manifest path was accepted")
	}

	assets[0].(map[string]any)["path"] = "payload/.goobers/agent-toolkit/manifest.json"
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeManifest(data); err == nil {
		t.Fatal("self-owned manifest path was accepted")
	}
}

func testBundle(t *testing.T, version, commit string) Bundle {
	t.Helper()
	bundle, err := Build(goobersassets.AgentToolkitAssets, version, commit)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}
