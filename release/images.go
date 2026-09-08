package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var operatorBuildPackage = "./cmd/operator"

type imageContextMetadata struct {
	SchemaVersion int    `json:"schemaVersion"`
	Kind          string `json:"kind"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Date          string `json:"date"`
	Platform      string `json:"platform"`
}

// imageContexts prepares build inputs only. It deliberately does not invoke a
// container engine, assign an image tag, publish, sign, or alter support tiers.
// The destination appears only after all release outputs have succeeded.
type imageContexts struct {
	temporary    string
	destination  string
	dockerfile   []byte
	windowsFiles map[string][]byte
	built        bool
}

func validateImageContextOptions(opts options, targetCSV string) error {
	if opts.imageContexts == "" {
		return nil
	}
	if strings.TrimSpace(targetCSV) == "" {
		return fmt.Errorf("-image-contexts requires explicit supported -targets")
	}
	if opts.skipUnbuildable {
		return fmt.Errorf("-image-contexts cannot be combined with -skip-unbuildable: every requested image context must build")
	}
	seen := make(map[Target]bool)
	for _, target := range opts.targets {
		if !supportedImageTarget(target) {
			return fmt.Errorf("image context target %s is unsupported; use linux/amd64, linux/arm64 or windows/amd64", target)
		}
		if target.OS == "windows" && (opts.commit == "" || strings.Trim(opts.commit, "0123456789abcdef") != "") {
			return fmt.Errorf("image inputs for Windows require a hexadecimal embedded commit stamp")
		}
		if seen[target] {
			return fmt.Errorf("duplicate image context target %s", target)
		}
		seen[target] = true
	}
	return nil
}

func supportedImageTarget(target Target) bool {
	return (target.OS == "linux" && (target.Arch == "amd64" || target.Arch == "arm64")) ||
		(target.OS == "windows" && target.Arch == "amd64")
}

func prepareImageContexts(opts options) (*imageContexts, error) {
	if opts.imageContexts == "" {
		return nil, nil
	}
	destination, err := filepath.Abs(opts.imageContexts)
	if err != nil {
		return nil, fmt.Errorf("resolve image context destination: %w", err)
	}
	output, err := filepath.Abs(opts.outDir)
	if err != nil {
		return nil, fmt.Errorf("resolve release output directory: %w", err)
	}
	if relative, err := filepath.Rel(destination, output); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("release output directory must not be inside the image context destination")
	}
	if err := requireAbsentImageDestination(destination); err != nil {
		return nil, err
	}
	repoRoot, err := findAgentToolkitRepositoryRoot()
	if err != nil {
		return nil, err
	}
	dockerfile, err := os.ReadFile(filepath.Join(repoRoot, "packaging", "docker", "base", "Dockerfile"))
	if err != nil {
		return nil, fmt.Errorf("read base-image Dockerfile: %w", err)
	}
	windowsFiles, err := windowsImageMaterials(repoRoot, opts.targets)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create image context parent: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".goobers-image-contexts-")
	if err != nil {
		return nil, fmt.Errorf("stage image contexts: %w", err)
	}
	return &imageContexts{temporary: temporary, destination: destination, dockerfile: dockerfile, windowsFiles: windowsFiles}, nil
}

func requireAbsentImageDestination(destination string) error {
	_, err := os.Lstat(destination)
	if err == nil {
		return fmt.Errorf("image context destination already exists: %s; choose a new directory", destination)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect image context destination: %w", err)
	}
	return nil
}

func (images *imageContexts) cleanup() {
	if images != nil {
		_ = os.RemoveAll(images.temporary)
	}
}

func (images *imageContexts) stage(target Target, binary, ldflags string, opts options) error {
	if images == nil {
		return nil
	}
	directory := filepath.Join(images.temporary, target.OS+"-"+target.Arch)
	if err := os.Mkdir(directory, 0o755); err != nil {
		return fmt.Errorf("stage image context %s: %w", target, err)
	}
	// Copy the exact archive input; rebuilding goobers independently would lose
	// byte equality and make image/archive skew possible.
	data, err := os.ReadFile(binary)
	if err != nil {
		return fmt.Errorf("read image binary for %s: %w", target, err)
	}
	if err := os.WriteFile(filepath.Join(directory, target.binaryName()), data, 0o755); err != nil {
		return fmt.Errorf("copy image binary for %s: %w", target, err)
	}
	output, err := buildReleaseBinary(target, ldflags, filepath.Join(directory, imageOperatorName(target)), operatorBuildPackage)
	if err != nil {
		return fmt.Errorf("build image operator %s: %w\n%s", target, err, output)
	}
	metadata := imageContextMetadata{
		SchemaVersion: 1, Kind: "goobers-base-build-inputs", Version: opts.version,
		Commit: opts.commit, Date: opts.date, Platform: target.String(),
	}
	files := map[string][]byte{"Dockerfile": images.dockerfile}
	if target.OS == "windows" {
		files = images.windowsFiles
		if err := prepareWindowsImageDependencies(directory, files["dependencies.json"]); err != nil {
			return err
		}
	}
	return writeImageContextFiles(directory, files, metadata)
}

func imageOperatorName(target Target) string {
	if target.OS == "windows" {
		return "goobers-operator.exe"
	}
	return "goobers-operator"
}

func writeImageContextFiles(directory string, files map[string][]byte, metadata imageContextMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode image context metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "release.json"), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write image context metadata: %w", err)
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(directory, name), contents, 0o644); err != nil {
			return fmt.Errorf("write image context %s: %w", name, err)
		}
	}
	return writeImageContextChecksums(directory)
}

func writeImageContextChecksums(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read image context: %w", err)
	}
	var paths []string
	for _, entry := range entries {
		paths = append(paths, filepath.Join(directory, entry.Name()))
	}
	manifest, err := checksumsManifest(paths)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(manifest), 0o644); err != nil {
		return fmt.Errorf("write image context checksums: %w", err)
	}
	return nil
}

func (images *imageContexts) finalize(stdout io.Writer) error {
	if images == nil {
		return nil
	}
	if err := requireAbsentImageDestination(images.destination); err != nil {
		return err
	}
	if err := os.Chmod(images.temporary, 0o755); err != nil {
		return fmt.Errorf("set image context permissions: %w", err)
	}
	if err := os.Rename(images.temporary, images.destination); err != nil {
		return fmt.Errorf("finalize image contexts: %w", err)
	}
	if images.built {
		_, _ = fmt.Fprintf(stdout, "built and verified local images -> %s (not published or signed)\n", images.destination)
	} else {
		_, _ = fmt.Fprintf(stdout, "prepared base-image build inputs -> %s (images not built or published)\n", images.destination)
	}
	return nil
}
