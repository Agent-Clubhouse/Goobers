package portalextension

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func testBundle(t *testing.T, version string) Bundle {
	return testBundleWithSource(t, version, "export {};\n")
}

func testBundleWithSource(t *testing.T, version, extension string) Bundle {
	t.Helper()
	source := fstest.MapFS{
		sourceRoot + "/extension.mjs":          {Data: []byte(extension)},
		sourceRoot + "/goober-mascot.png":      {Data: []byte("png")},
		sourceRoot + "/copilot-extension.json": {Data: []byte(`{"name":"goobers-portal","version":1}`)},
	}
	bundle, err := Build(source, version, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestCheckFindsBundledAssetChangesWithSameReleaseIdentity(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(testBundleWithSource(t, "dev", "old\n")); err != nil {
		t.Fatal(err)
	}
	changed := testBundleWithSource(t, "dev", "new\n")
	report, err := manager.Check(changed)
	if err != nil || report.State != "update-available" {
		t.Fatalf("Check = %+v, %v", report, err)
	}
	if _, err := manager.Update(changed, false); err != nil {
		t.Fatal(err)
	}
}

func TestInstallCheckAndUpdate(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}
	oldBundle := testBundle(t, "v1.0.0")
	result, err := manager.Install(oldBundle)
	if err != nil || !result.Installed {
		t.Fatalf("Install = %+v, %v", result, err)
	}
	report, err := manager.Check(oldBundle)
	if err != nil || report.State != "current" {
		t.Fatalf("Check = %+v, %v", report, err)
	}

	newBundle := testBundle(t, "v1.1.0")
	report, err = manager.Check(newBundle)
	if err != nil || report.State != "update-available" {
		t.Fatalf("update Check = %+v, %v", report, err)
	}
	result, err = manager.Update(newBundle, false)
	if err != nil || !result.Installed {
		t.Fatalf("Update = %+v, %v", result, err)
	}
	report, err = manager.Check(newBundle)
	if err != nil || report.State != "current" || report.InstalledVersion != "v1.1.0" {
		t.Fatalf("post-update Check = %+v, %v", report, err)
	}
}

func TestUpdateRequiresAcknowledgementForModifiedFiles(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}

	bundle := testBundle(t, "v1.0.0")
	if _, err := manager.Install(bundle); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(manager.Path(), "extension.mjs")
	if err := os.WriteFile(extension, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update(testBundle(t, "v1.1.0"), false); err == nil {
		t.Fatal("Update replaced a locally modified file without acknowledgement")
	}
	if got, err := os.ReadFile(extension); err != nil || string(got) != "local edit\n" {
		t.Fatalf("modified file = %q, %v", got, err)
	}
	if _, err := manager.Update(testBundle(t, "v1.1.0"), true); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRequiresAcknowledgementWhenManifestWasEditedToHideChanges(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := testBundle(t, "v1.0.0")
	if _, err := manager.Install(bundle); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(manager.Path(), "extension.mjs")
	localEdit := []byte("local edit\n")
	if err := os.WriteFile(extension, localEdit, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(manager.Path(), manifestName)
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(localEdit)
	for index := range manifest.Assets {
		if manifest.Assets[index].Path == "extension.mjs" {
			manifest.Assets[index].SHA256 = fmt.Sprintf("%x", sum)
		}
	}
	manifestData, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(manifestData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := manager.Check(testBundle(t, "v1.1.0"))
	if err != nil || report.State != "modified" {
		t.Fatalf("Check = %+v, %v", report, err)
	}
	if _, err := manager.Update(testBundle(t, "v1.1.0"), false); err == nil {
		t.Fatal("Update replaced changes hidden by an edited manifest without acknowledgement")
	}
}

func TestUpdateRepairsOwnedFileReplacedByDirectory(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := testBundle(t, "v1.0.0")
	if _, err := manager.Install(bundle); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(manager.Path(), "extension.mjs")
	if err := os.Remove(extension); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(extension, 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := manager.Check(bundle)
	if err != nil || report.State != "modified" {
		t.Fatalf("Check = %+v, %v", report, err)
	}
	if _, err := manager.Update(bundle, true); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(extension); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("repaired extension = %v, %v", info, err)
	}
}

func TestCheckRecoversInterruptedDirectorySwap(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := testBundle(t, "v1.0.0")
	if _, err := manager.Install(bundle); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manager.Path(), manager.Path()+".previous"); err != nil {
		t.Fatal(err)
	}
	report, err := manager.Check(bundle)
	if err != nil || report.State != "current" {
		t.Fatalf("recovered Check = %+v, %v", report, err)
	}
	if _, err := os.Stat(manager.Path()); err != nil {
		t.Fatalf("active extension was not restored: %v", err)
	}
}

func TestUpdateMigratesLegacyStateOutsideCodeDirectory(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Install(testBundle(t, "v1.0.0")); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(manager.Path(), legacyStateDir)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "sources.json"), []byte(`{"sources":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update(testBundle(t, "v1.1.0"), false); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(manager.home, stateDirectory, Name, "sources.json")
	if got, err := os.ReadFile(state); err != nil || string(got) != `{"sources":[]}` {
		t.Fatalf("migrated state = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(manager.Path(), legacyStateDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("legacy state remained in installed code directory: %v", err)
	}
}

func TestRecoveryMigratesNewerLegacyStateFromPreviousExtension(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}
	oldBundle := testBundle(t, "v1.0.0")
	if _, err := manager.Install(oldBundle); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(manager.home, stateDirectory, Name, "sources.json")
	if err := os.WriteFile(state, []byte(`{"sources":["old"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := manager.Path() + ".previous"
	if err := os.Rename(manager.Path(), previous); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(previous, legacyStateDir, "sources.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"sources":["new"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	newTime := oldTime.Add(time.Second)
	if err := os.Chtimes(state, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(legacy, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	newBundle := testBundle(t, "v1.1.0")
	if err := os.MkdirAll(manager.Path(), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range newBundle.Files {
		target := filepath.Join(manager.Path(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(manager.Path(), manifestName), newBundle.ManifestJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manager.writeDigestRecord(manager.pendingRecordPath(), newBundle.ManifestJSON); err != nil {
		t.Fatal(err)
	}

	report, err := manager.Check(newBundle)
	if err != nil || report.State != "current" {
		t.Fatalf("Check = %+v, %v", report, err)
	}
	if got, err := os.ReadFile(state); err != nil || string(got) != `{"sources":["new"]}` {
		t.Fatalf("reconciled state = %q, %v", got, err)
	}
	if _, err := os.Stat(previous); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("previous extension remained after recovery: %v", err)
	}
}

func TestRecoveryRefusesActiveManifestWithoutMatchingPendingUpdate(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}
	oldBundle := testBundle(t, "v1.0.0")
	if _, err := manager.Install(oldBundle); err != nil {
		t.Fatal(err)
	}
	previous := manager.Path() + ".previous"
	if err := os.Rename(manager.Path(), previous); err != nil {
		t.Fatal(err)
	}
	tampered := testBundleWithSource(t, "v1.1.0", "tampered\n")
	if err := os.MkdirAll(manager.Path(), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range tampered.Files {
		target := filepath.Join(manager.Path(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(manager.Path(), manifestName), tampered.ManifestJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	intended := testBundle(t, "v1.1.0")
	if err := manager.writeDigestRecord(manager.pendingRecordPath(), intended.ManifestJSON); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Check(intended); err == nil {
		t.Fatal("recovery trusted an active manifest that did not match the pending update")
	}
	if _, err := os.Stat(previous); err != nil {
		t.Fatalf("recovery backup was not preserved: %v", err)
	}
}

func TestInstallRefusesUnmanagedDirectory(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), ".copilot"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.Path(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.Path(), "mine.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(testBundle(t, "v1.0.0")); err == nil {
		t.Fatal("Install claimed an unmanaged extension directory")
	}
}

func TestDetectCopilotAppWith(t *testing.T) {
	notFound := func(string) (string, error) { return "", fs.ErrNotExist }
	if DetectCopilotAppWith("linux", "", "", notFound) {
		t.Fatal("Linux detection succeeded without an executable")
	}
	found := func(name string) (string, error) {
		if name != "github" {
			t.Fatalf("LookPath(%q), want github", name)
		}
		return "/bin/github", nil
	}
	if !DetectCopilotAppWith("linux", "", "", found) {
		t.Fatal("PATH detection did not find Copilot app")
	}
	local := t.TempDir()
	app := filepath.Join(local, "Programs", "GitHub Copilot", "github.exe")
	if err := os.MkdirAll(filepath.Dir(app), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !DetectCopilotAppWith("windows", "", local, notFound) {
		t.Fatal("Windows installation path was not detected")
	}
}
