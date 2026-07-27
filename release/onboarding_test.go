package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestOnboardingPayloadPinsReleaseSources(t *testing.T) {
	repoRoot := agentToolkitRepoRoot(t)
	payloadRoot := filepath.Join(t.TempDir(), onboardingRoot)
	manifest, err := stageOnboardingPayload(repoRoot, "v1.2.3", "deadbeef", payloadRoot)
	if err != nil {
		t.Fatal(err)
	}

	if manifest.Release != (onboardingRelease{Version: "v1.2.3", Commit: "deadbeef"}) {
		t.Errorf("release = %+v", manifest.Release)
	}
	if len(manifest.Templates) != 2 ||
		manifest.Templates[0] != (onboardingTemplate{ID: "canonical", Version: "v1.2.3", Path: "templates/canonical"}) ||
		manifest.Templates[1] != (onboardingTemplate{ID: "quickstart", Version: "v1", Path: "templates/quickstart@v1"}) {
		t.Errorf("templates = %+v", manifest.Templates)
	}
	if len(manifest.Samples) != 1 {
		t.Fatalf("samples = %+v", manifest.Samples)
	}
	sample := manifest.Samples[0]
	if sample.ID != "getting-started-task-api" || sample.Version != "1.0.0" ||
		sample.Path != "samples/getting-started-task-api@1.0.0" ||
		sample.SeedIssues != "samples/getting-started-task-api@1.0.0/seed-issues.json" {
		t.Errorf("sample = %+v", sample)
	}

	manifestData, err := os.ReadFile(filepath.Join(payloadRoot, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded onboardingManifest
	if err := json.Unmarshal(manifestData, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Release != manifest.Release || len(decoded.Files) != len(manifest.Files) {
		t.Errorf("written manifest differs from staged manifest")
	}

	for _, file := range manifest.Files {
		bundled, err := os.ReadFile(filepath.Join(payloadRoot, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Errorf("read bundled %s: %v", file.Path, err)
			continue
		}
		source := onboardingSourcePath(t, repoRoot, file.Path)
		want, err := os.ReadFile(source)
		if err != nil {
			t.Errorf("read source for %s: %v", file.Path, err)
			continue
		}
		if !bytes.Equal(bundled, want) {
			t.Errorf("bundled %s differs from %s", file.Path, source)
		}
		sum := sha256.Sum256(bundled)
		if got := fmt.Sprintf("%x", sum); got != file.SHA256 {
			t.Errorf("%s digest = %s, want %s", file.Path, got, file.SHA256)
		}
		if int64(len(bundled)) != file.Size {
			t.Errorf("%s size = %d, want %d", file.Path, len(bundled), file.Size)
		}
		sourceInfo, err := os.Stat(source)
		if err != nil {
			t.Errorf("stat source for %s: %v", file.Path, err)
			continue
		}
		bundledInfo, err := os.Stat(filepath.Join(payloadRoot, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Errorf("stat bundled %s: %v", file.Path, err)
			continue
		}
		if bundledInfo.Mode().Perm() != sourceInfo.Mode().Perm() ||
			file.Mode != fmt.Sprintf("%04o", sourceInfo.Mode().Perm()) {
			t.Errorf(
				"%s modes: staged=%04o manifest=%s source=%04o",
				file.Path,
				bundledInfo.Mode().Perm(),
				file.Mode,
				sourceInfo.Mode().Perm(),
			)
		}
	}
	if _, err := os.Stat(filepath.Join(payloadRoot, "templates", "canonical", "embed.go")); !os.IsNotExist(err) {
		t.Errorf("canonical payload contains Go embedding source: %v", err)
	}

	instanceRoot := filepath.Join(t.TempDir(), "quickstart")
	if _, err := instance.InitQuickstart(instanceRoot); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(filepath.Join(instanceRoot, "config"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		relative, err := filepath.Rel(filepath.Join(instanceRoot, "config"), path)
		if err != nil {
			return err
		}
		emitted, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		bundled, err := os.ReadFile(filepath.Join(payloadRoot, "templates", "quickstart@v1", relative))
		if err != nil {
			return err
		}
		if !bytes.Equal(emitted, bundled) {
			t.Errorf("installed quickstart file %s differs from release payload", relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	archivePath, err := packageOnboardingArchive(payloadRoot, "v1.2.3", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(archivePath), "goobers-onboarding_v1.2.3.zip"; got != want {
		t.Fatalf("archive name = %q, want %q", got, want)
	}
	entries := readAgentToolkitArchive(t, archivePath)
	if len(entries) != len(manifest.Files)+1 {
		t.Errorf("archive entries = %d, want %d", len(entries), len(manifest.Files)+1)
	}
	for name, archived := range entries {
		staged, err := os.ReadFile(filepath.Join(payloadRoot, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("read staged %s: %v", name, err)
			continue
		}
		if !bytes.Equal(archived, staged) {
			t.Errorf("standalone archive %s differs from platform payload", name)
		}
	}
}

func onboardingSourcePath(t *testing.T, repoRoot, bundledPath string) string {
	t.Helper()
	sources := []struct {
		prefix string
		source string
	}{
		{"templates/canonical/", "config-examples"},
		{"templates/quickstart@v1/", "internal/instance/quickstart-v1"},
		{"samples/getting-started-task-api@1.0.0/", "samples/getting-started-task-api"},
	}
	for _, candidate := range sources {
		if relative, ok := strings.CutPrefix(bundledPath, candidate.prefix); ok {
			return filepath.Join(repoRoot, filepath.FromSlash(candidate.source), filepath.FromSlash(relative))
		}
	}
	t.Fatalf("unknown onboarding path %q", bundledPath)
	return ""
}
