package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func validateImageImportFlags(opts options, fs *flag.FlagSet) error {
	if opts.imageArtifacts == "" && opts.imageInputs == "" {
		return nil
	}
	if opts.imageArtifacts == "" || opts.imageInputs == "" || opts.imageContexts == "" {
		return fmt.Errorf("final artifact import requires -image-artifacts, -image-inputs and -image-contexts")
	}
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	for _, name := range []string{"version", "commit", "date", "targets"} {
		if !seen[name] {
			return fmt.Errorf("final artifact import requires explicit -%s", name)
		}
	}
	for _, name := range []string{"output", "previous-features", "previous-support-matrix", "first-feature-snapshot", "checksums", "skip-unbuildable"} {
		if seen[name] {
			return fmt.Errorf("-%s does not apply to final artifact import", name)
		}
	}
	if opts.commit == "" || strings.Trim(opts.commit, "0123456789abcdef") != "" {
		return fmt.Errorf("final artifact import requires a hexadecimal embedded commit stamp")
	}
	if _, err := time.Parse(time.RFC3339, opts.date); err != nil {
		return fmt.Errorf("final artifact import requires an RFC3339 build date: %w", err)
	}
	return nil
}

// Import consumes the final archive and original prepared operator bytes. It
// never invokes Go builds, regenerates release assets, or signs an executable.
// The caller must supply trusted artifacts; checksums are integrity checks,
// not signature verification. Native image verification checks runtime stamps.
func runImageArtifactImport(opts options, stdout io.Writer) error {
	if err := validateImageImportPaths(opts); err != nil {
		return err
	}
	images, err := prepareImageContexts(opts)
	if err != nil {
		return err
	}
	defer images.cleanup()
	for _, target := range opts.targets {
		if err := images.importFinalArtifact(opts, target); err != nil {
			return err
		}
	}
	plan, err := prepareLocalImageBuild(opts)
	if err != nil {
		return err
	}
	if err := buildLocalReleaseImages(plan, images, opts, stdout); err != nil {
		return err
	}
	return images.finalize(stdout)
}

func validateImageImportPaths(opts options) error {
	parent, err := filepath.EvalSymlinks(filepath.Dir(opts.imageContexts))
	if err != nil {
		return fmt.Errorf("image import destination parent must exist: %w", err)
	}
	destination, err := filepath.Abs(filepath.Join(parent, filepath.Base(opts.imageContexts)))
	if err != nil {
		return err
	}
	for _, input := range []string{opts.imageArtifacts, opts.imageInputs} {
		source, err := filepath.EvalSymlinks(input)
		if err != nil {
			return fmt.Errorf("resolve image artifact input: %w", err)
		}
		source, err = filepath.Abs(source)
		if err != nil {
			return err
		}
		if imagePathsOverlap(source, destination) {
			return fmt.Errorf("image import destination must be separate from its input directories")
		}
	}
	return nil
}

func imagePathsOverlap(a, b string) bool {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

type imageArtifactImportEvidence struct {
	SchemaVersion            int               `json:"schemaVersion"`
	Scope                    string            `json:"scope"`
	ArchiveName              string            `json:"archiveName"`
	ArchiveSHA256            string            `json:"archiveSHA256"`
	OriginalContextChecksums map[string]string `json:"originalContextChecksums"`
}

func (images *imageContexts) importFinalArtifact(opts options, target Target) error {
	original := filepath.Join(opts.imageInputs, target.OS+"-"+target.Arch)
	manifest, err := readImageArtifactManifest(original)
	if err != nil {
		return err
	}
	files, err := images.readOriginalImageInputs(original, manifest, target)
	if err != nil {
		return err
	}
	metadata := imageContextMetadata{1, "goobers-base-build-inputs", opts.version, opts.commit, opts.date, target.String()}
	expected, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if !bytes.Equal(files["release.json"], append(expected, '\n')) {
		return fmt.Errorf("original image context %s does not match expected release metadata", target)
	}
	archive, err := readFinalArchiveBinary(opts.imageArtifacts, opts.version, target)
	if err != nil {
		return err
	}
	files[target.binaryName()] = archive.Data
	evidence := imageArtifactImportEvidence{1, "final archive bytes and original operator inputs; signatures and embedded runtime stamps require separate native verification", archive.ArchiveName, archive.ArchiveSHA256, manifest}
	proof, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	files["artifact-import.json"] = append(proof, '\n')
	directory := filepath.Join(images.temporary, target.OS+"-"+target.Arch)
	if err := os.Mkdir(directory, 0o755); err != nil {
		return err
	}
	for name, data := range files {
		mode := os.FileMode(0o644)
		if name == target.binaryName() || name == imageOperatorName(target) {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, mode); err != nil {
			return err
		}
	}
	return writeImageContextChecksums(directory)
}

func (images *imageContexts) readOriginalImageInputs(directory string, manifest map[string]string, target Target) (map[string][]byte, error) {
	recipes := map[string][]byte{"Dockerfile": images.dockerfile}
	limits := map[string]int64{target.binaryName(): 128 << 20, imageOperatorName(target): 128 << 20, "release.json": 64 << 10}
	if target.OS == "windows" {
		recipes = images.windowsFiles
		limits["mingit.zip"], limits["zoneinfo.zip"] = 128<<20, 4<<20
	}
	for name := range recipes {
		limits[name] = 1 << 20
	}
	if len(manifest) != len(limits) {
		return nil, fmt.Errorf("original image context has missing or unexpected checksummed inputs")
	}
	files := make(map[string][]byte, len(limits))
	for name, limit := range limits {
		data, err := readVerifiedImageArtifact(directory, name, manifest, limit)
		if err != nil {
			return nil, err
		}
		if recipe, ok := recipes[name]; ok && !bytes.Equal(data, recipe) {
			return nil, fmt.Errorf("original image recipe %s differs from the checked-out release source", name)
		}
		files[name] = data
	}
	if target.OS == "windows" {
		if err := verifyImportedWindowsDependencies(files); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func verifyImportedWindowsDependencies(files map[string][]byte) error {
	pins, err := parseWindowsImageDependencies(files["dependencies.json"])
	if err != nil {
		return err
	}
	for name, pin := range map[string]imageDependency{"mingit.zip": pins.MinGit, "zoneinfo.zip": pins.ZoneInfo} {
		sum := sha256.Sum256(files[name])
		if !strings.EqualFold(hex.EncodeToString(sum[:]), pin.SHA256) {
			return fmt.Errorf("imported image dependency %s differs from its source pin", name)
		}
	}
	return nil
}
