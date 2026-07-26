package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	onboardingSchemaVersion = 1
	onboardingRoot          = "onboarding"
)

type onboardingManifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Release       onboardingRelease    `json:"release"`
	Templates     []onboardingTemplate `json:"templates"`
	Samples       []onboardingSample   `json:"samples"`
	Files         []onboardingFile     `json:"files"`
}

type onboardingRelease struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type onboardingTemplate struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Path    string `json:"path"`
}

type onboardingSample struct {
	ID                  string   `json:"id"`
	Version             string   `json:"version"`
	Path                string   `json:"path"`
	SeedIssues          string   `json:"seedIssues"`
	CompatibleTemplates []string `json:"compatibleTemplates"`
}

type onboardingFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
}

type onboardingPayloadFile struct {
	onboardingFile
	data []byte
	mode fs.FileMode
}

type onboardingSampleMetadata struct {
	SchemaVersion       int      `json:"schemaVersion"`
	ID                  string   `json:"id"`
	Version             string   `json:"version"`
	CompatibleTemplates []string `json:"compatibleTemplates"`
	SeedIssues          string   `json:"seedIssues"`
}

func stageOnboardingPayload(repoRoot, version, commit, destination string) (onboardingManifest, error) {
	version = strings.TrimSpace(version)
	commit = strings.TrimSpace(commit)
	if version == "" || commit == "" {
		return onboardingManifest{}, fmt.Errorf("onboarding release version and commit must not be empty")
	}

	sample, err := readOnboardingSampleMetadata(repoRoot)
	if err != nil {
		return onboardingManifest{}, err
	}
	samplePath := path.Join("samples", sample.ID+"@"+sample.Version)
	sources := []struct {
		source      string
		destination string
		include     func(string) bool
	}{
		{
			source:      "config-examples",
			destination: "templates/canonical",
			include: func(relative string) bool {
				return !strings.HasSuffix(relative, ".go")
			},
		},
		{
			source:      "internal/instance/quickstart-v1",
			destination: "templates/quickstart@v1",
			include:     includeOnboardingFile,
		},
		{
			source:      "samples/getting-started-task-api",
			destination: samplePath,
			include:     includeOnboardingFile,
		},
	}

	var files []onboardingPayloadFile
	for _, source := range sources {
		collected, err := collectOnboardingFiles(repoRoot, source.source, source.destination, source.include)
		if err != nil {
			return onboardingManifest{}, err
		}
		files = append(files, collected...)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for i, file := range files {
		if i > 0 && files[i-1].Path == file.Path {
			return onboardingManifest{}, fmt.Errorf("duplicate onboarding file %q", file.Path)
		}
	}

	manifest := onboardingManifest{
		SchemaVersion: onboardingSchemaVersion,
		Release:       onboardingRelease{Version: version, Commit: commit},
		Templates: []onboardingTemplate{
			{ID: "canonical", Version: version, Path: "templates/canonical"},
			{ID: "quickstart", Version: "v1", Path: "templates/quickstart@v1"},
		},
		Samples: []onboardingSample{{
			ID:                  sample.ID,
			Version:             sample.Version,
			Path:                samplePath,
			SeedIssues:          path.Join(samplePath, sample.SeedIssues),
			CompatibleTemplates: append([]string(nil), sample.CompatibleTemplates...),
		}},
		Files: make([]onboardingFile, len(files)),
	}
	for i, file := range files {
		manifest.Files[i] = file.onboardingFile
	}

	if err := os.MkdirAll(destination, 0o755); err != nil {
		return onboardingManifest{}, fmt.Errorf("create onboarding payload %s: %w", destination, err)
	}
	for _, file := range files {
		target := filepath.Join(destination, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return onboardingManifest{}, fmt.Errorf("create onboarding parent %s: %w", target, err)
		}
		if err := os.WriteFile(target, file.data, file.mode); err != nil {
			return onboardingManifest{}, fmt.Errorf("write onboarding file %s: %w", target, err)
		}
		if err := os.Chmod(target, file.mode); err != nil {
			return onboardingManifest{}, fmt.Errorf("set onboarding file mode %s: %w", target, err)
		}
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return onboardingManifest{}, fmt.Errorf("encode onboarding manifest: %w", err)
	}
	manifestJSON = append(manifestJSON, '\n')
	if err := os.WriteFile(filepath.Join(destination, "manifest.json"), manifestJSON, 0o644); err != nil {
		return onboardingManifest{}, fmt.Errorf("write onboarding manifest: %w", err)
	}
	if err := os.Chmod(filepath.Join(destination, "manifest.json"), 0o644); err != nil {
		return onboardingManifest{}, fmt.Errorf("set onboarding manifest mode: %w", err)
	}
	return manifest, nil
}

func readOnboardingSampleMetadata(repoRoot string) (onboardingSampleMetadata, error) {
	const metadataPath = "samples/getting-started-task-api/sample.json"
	data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(metadataPath)))
	if err != nil {
		return onboardingSampleMetadata{}, fmt.Errorf("read onboarding sample metadata: %w", err)
	}
	var sample onboardingSampleMetadata
	if err := json.Unmarshal(data, &sample); err != nil {
		return onboardingSampleMetadata{}, fmt.Errorf("decode onboarding sample metadata: %w", err)
	}
	if sample.SchemaVersion != onboardingSchemaVersion {
		return onboardingSampleMetadata{}, fmt.Errorf(
			"onboarding sample schema version is %d, want %d",
			sample.SchemaVersion,
			onboardingSchemaVersion,
		)
	}
	for label, value := range map[string]string{
		"id":          sample.ID,
		"version":     sample.Version,
		"seed issues": sample.SeedIssues,
	} {
		if !validOnboardingPathSegment(value) {
			return onboardingSampleMetadata{}, fmt.Errorf("onboarding sample %s %q is not a portable path segment", label, value)
		}
	}
	quickstartCompatible := false
	for _, template := range sample.CompatibleTemplates {
		if template == "quickstart@v1" {
			quickstartCompatible = true
			break
		}
	}
	if !quickstartCompatible {
		return onboardingSampleMetadata{}, fmt.Errorf("onboarding sample must be compatible with quickstart@v1")
	}
	return sample, nil
}

func validOnboardingPathSegment(value string) bool {
	return value != "" && value != "." && value != ".." &&
		!strings.ContainsAny(value, `/\`)
}

func includeOnboardingFile(relative string) bool {
	first, _, _ := strings.Cut(relative, "/")
	return first != ".git" && first != "dist" && first != "node_modules"
}

func collectOnboardingFiles(
	repoRoot, sourceDir, destinationDir string,
	include func(string) bool,
) ([]onboardingPayloadFile, error) {
	root := filepath.Join(repoRoot, filepath.FromSlash(sourceDir))
	var files []onboardingPayloadFile
	err := filepath.WalkDir(root, func(sourcePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, sourcePath)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !include(relative) {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("onboarding source %s/%s is a symbolic link", sourceDir, relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("onboarding source %s/%s is not a regular file", sourceDir, relative)
		}
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files = append(files, onboardingPayloadFile{
			onboardingFile: onboardingFile{
				Path:   path.Join(destinationDir, relative),
				SHA256: fmt.Sprintf("%x", sum),
				Size:   int64(len(data)),
				Mode:   fmt.Sprintf("%04o", info.Mode().Perm()),
			},
			data: data,
			mode: info.Mode().Perm(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collect onboarding source %s: %w", sourceDir, err)
	}
	return files, nil
}

func onboardingArchiveName(version string) string {
	return fmt.Sprintf("goobers-onboarding_%s.zip", version)
}

func packageOnboardingArchive(root, version, outDir string) (archivePath string, retErr error) {
	entries, err := collectArchiveEntries(root)
	if err != nil {
		return "", err
	}
	archivePath = filepath.Join(outDir, onboardingArchiveName(version))
	file, err := os.Create(archivePath)
	if err != nil {
		return "", fmt.Errorf("create onboarding archive %s: %w", archivePath, err)
	}
	defer func() {
		if err := file.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close onboarding archive %s: %w", archivePath, err)
		}
		if retErr != nil {
			_ = os.Remove(archivePath)
		}
	}()

	if err := writeZip(file, entries); err != nil {
		return "", fmt.Errorf("write onboarding archive %s: %w", archivePath, err)
	}
	return archivePath, nil
}
