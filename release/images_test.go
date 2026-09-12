package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/buildinfo"
	"debug/elf"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestImageContextFlagsRefuseUnsupportedRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"implicit full matrix", nil},
		{"unsupported Windows architecture", []string{"-targets", "windows/arm64"}},
		{"nonhex Windows stamp", []string{"-targets", "windows/amd64", "-commit", "not-a-commit"}},
		{"darwin", []string{"-targets", "darwin/arm64"}},
		{"mixed unsupported platforms", []string{"-targets", "linux/amd64,windows/arm64"}},
		{"unsupported architecture", []string{"-targets", "linux/386"}},
		{"duplicate target", []string{"-targets", "linux/amd64,linux/amd64"}},
		{"skip builds", []string{"-targets", "linux/amd64", "-skip-unbuildable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			args := []string{"-first-feature-snapshot", "-image-contexts", filepath.Join(root, "images"), "-output", filepath.Join(root, "assets")}
			if err := run(append(args, tc.args...), io.Discard, io.Discard); err == nil {
				t.Fatal("unsupported image preparation unexpectedly succeeded")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("unsupported request performed work: entries=%v, error=%v", entries, err)
			}
		})
	}
	if _, err := parseFlags([]string{"-first-feature-snapshot", "-image-contexts", "images", "-targets", "linux/amd64,linux/arm64,windows/amd64"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func useImageTestBinaries(t *testing.T) {
	t.Helper()
	originalBuild, originalOperator, originalPortal := buildPackage, operatorBuildPackage, portalAssetsDirectory
	buildPackage, operatorBuildPackage = "./testdata/image-binary", "./testdata/image-binary"
	portalAssetsDirectory = t.TempDir()
	if err := os.WriteFile(filepath.Join(portalAssetsDirectory, "index.html"), []byte("portal"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		buildPackage, operatorBuildPackage, portalAssetsDirectory = originalBuild, originalOperator, originalPortal
	})
}

func imageTestArgs(root string) []string {
	return []string{
		"-version", "v0.4.0-rc.1", "-commit", "0123456789abcdef0123456789abcdef01234567", "-date", "2026-09-07T12:00:00Z",
		"-first-feature-snapshot", "-targets", "linux/amd64,linux/arm64", "-checksums=false",
		"-output", filepath.Join(root, "assets"), "-image-contexts", filepath.Join(root, "images"),
	}
}

func TestReleaseImageContextsMatchArchiveInputs(t *testing.T) {
	useImageTestBinaries(t)
	root := t.TempDir()
	if err := run(imageTestArgs(root), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			context := filepath.Join(root, "images", "linux-"+arch)
			archive := filepath.Join(root, "assets", "goobers_v0.4.0-rc.1_linux_"+arch+".tar.gz")
			binary, err := os.ReadFile(filepath.Join(context, "goobers"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(binary, archiveImageBinary(t, archive)) {
				t.Fatal("image and archive binary bytes differ")
			}
			for _, name := range []string{"goobers", "goobers-operator"} {
				assertImageBinaryBuildInputs(t, filepath.Join(context, name), arch)
			}
			metadata, err := os.ReadFile(filepath.Join(context, "release.json"))
			if err != nil {
				t.Fatal(err)
			}
			var got imageContextMetadata
			if err := json.Unmarshal(metadata, &got); err != nil {
				t.Fatal(err)
			}
			want := imageContextMetadata{1, "goobers-base-build-inputs", "v0.4.0-rc.1", "0123456789abcdef0123456789abcdef01234567", "2026-09-07T12:00:00Z", "linux/" + arch}
			if got != want {
				t.Fatalf("metadata = %+v, want %+v", got, want)
			}
			var paths []string
			for _, name := range []string{"Dockerfile", "release.json", "goobers", "goobers-operator"} {
				paths = append(paths, filepath.Join(context, name))
			}
			manifest, err := checksumsManifest(paths)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(filepath.Join(context, "SHA256SUMS"))
			if err != nil || string(actual) != manifest {
				t.Fatalf("context checksum mismatch: %s, error=%v", actual, err)
			}
		})
	}
	assertNoImageStaging(t, root)
}

func assertImageBinaryBuildInputs(t *testing.T, path, arch string) {
	t.Helper()
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != "linux" || settings["GOARCH"] != arch || settings["CGO_ENABLED"] != "0" {
		t.Fatalf("unexpected build platform: %v", settings)
	}
	file, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	// -trimpath omits -ldflags from Go's build settings. Inspect the actual
	// linked read-only data instead; the fixture references version.Get(), so
	// these unique values must have replaced the package's dev/none/unknown.
	data, err := file.Section(".rodata").Data()
	if err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []string{"v0.4.0-rc.1", "0123456789abcdef0123456789abcdef01234567", "2026-09-07T12:00:00Z"} {
		if !bytes.Contains(data, []byte(stamp)) {
			t.Fatalf("%s missing linked build stamp %s", path, stamp)
		}
	}
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if file.Machine != want {
		t.Fatalf("ELF machine = %s, want %s", file.Machine, want)
	}
}

func archiveImageBinary(t *testing.T, path string) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("archive missing binary: %v", err)
		}
		if header.Name == "goobers" {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
}

func TestImageContextsRemovedOnBuildAndLaterReleaseFailure(t *testing.T) {
	for _, failure := range []string{"operator build", "later release packaging"} {
		t.Run(failure, func(t *testing.T) {
			useImageTestBinaries(t)
			if failure == "operator build" {
				operatorBuildPackage = "./testdata/nonexistent-image-operator"
			} else {
				portalAssetsDirectory = filepath.Join(t.TempDir(), "missing-portal-assets")
			}
			root := t.TempDir()
			err := run(imageTestArgs(root), io.Discard, io.Discard)
			if err == nil {
				t.Fatal("expected release failure")
			}
			if _, err := os.Lstat(filepath.Join(root, "images")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed release exposed image contexts: %v", err)
			}
			assertNoImageStaging(t, root)
		})
	}
}

func assertNoImageStaging(t *testing.T, root string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".goobers-image-contexts-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary image contexts remain: %v, error=%v", matches, err)
	}
}

func TestImageContextsRefuseExistingOrOverlappingDestination(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.WriteFile(existing, []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []options{
		{imageContexts: existing, outDir: filepath.Join(root, "assets")},
		{imageContexts: filepath.Join(root, "images"), outDir: filepath.Join(root, "images", "assets")},
	} {
		if images, err := prepareImageContexts(opts); err == nil {
			images.cleanup()
			t.Fatal("expected destination refusal")
		}
	}
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("existing destination changed: %s, error=%v", data, err)
	}
	assertNoImageStaging(t, root)
}

func TestImageContextsCopyCommittedDockerfile(t *testing.T) {
	root := t.TempDir()
	images, err := prepareImageContexts(options{imageContexts: filepath.Join(root, "images"), outDir: filepath.Join(root, "assets")})
	if err != nil {
		t.Fatal(err)
	}
	defer images.cleanup()
	repo, err := findAgentToolkitRepositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "packaging", "docker", "base", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(images.dockerfile, data) {
		t.Fatal("prepared Dockerfile differs from committed source")
	}
	if err := os.MkdirAll(images.destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := images.finalize(io.Discard); err == nil {
		t.Fatal("finalization overwrote a concurrently created destination")
	}
}
