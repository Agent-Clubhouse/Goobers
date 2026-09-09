package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestImageTargetSelectionValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing contexts", []string{"-image-contexts=", "-image-targets", "linux/arm64"}, "requires -image-contexts"},
		{"empty selection", []string{"-image-targets="}, "nonempty"},
		{"empty tokens", []string{"-image-targets", ",,"}, "no targets"},
		{"malformed target", []string{"-image-targets", "linux"}, "invalid target"},
		{"duplicate selection", []string{"-image-targets", "linux/arm64,linux/arm64"}, "duplicate image context"},
		{"absent archive", []string{"-targets", "darwin/arm64", "-image-targets", "linux/arm64"}, "exactly once in archive targets"},
		{"duplicate selected archive", []string{"-targets", "linux/arm64,linux/arm64", "-image-targets", "linux/arm64"}, "exactly once in archive targets"},
		{"Darwin image", []string{"-targets", "darwin/arm64", "-image-targets", "darwin/arm64"}, "unsupported"},
		{"Windows ARM image", []string{"-targets", "windows/arm64", "-image-targets", "windows/arm64"}, "unsupported"},
		{"skip archive failures", []string{"-image-targets", "linux/arm64", "-skip-unbuildable"}, "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			err := run(append(imageTestArgs(root), tc.args...), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want %s", err, tc.want)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid selection performed work: entries=%v error=%v", entries, err)
			}
		})
	}
}

func TestImageTargetSelectionDefaultsAndImport(t *testing.T) {
	args := append(imageTestArgs(t.TempDir()), "-targets=", "-image-targets", "linux/amd64,linux/arm64,windows/amd64")
	opts, err := parseFlags(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opts.targets, DefaultTargets) || len(imageTargetOptions(opts).targets) != 3 {
		t.Fatalf("selection changed the archive matrix: %+v", opts)
	}
	for _, selection := range []string{"", "linux/arm64"} {
		args := append(importTestArgs(t.TempDir(), "linux/arm64"), "-image-targets", selection)
		if _, err := parseFlags(args, io.Discard); err == nil || !strings.Contains(err.Error(), "cannot be combined with final artifact import") {
			t.Fatalf("import must reject -image-targets=%q: %v", selection, err)
		}
	}
}

func TestReleaseImageSubsetPreservesFullArchiveMatrix(t *testing.T) {
	useImageTestBinaries(t)
	useWindowsImageFixtures(t, false)
	root := t.TempDir()
	args := append(imageTestArgs(root), "-targets=", "-checksums=true", "-image-targets", "linux/amd64,linux/arm64,windows/amd64")
	if err := run(args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, target := range DefaultTargets {
		archived, err := readFinalArchiveBinary(filepath.Join(root, "assets"), "v0.4.0-rc.1", target)
		if err != nil {
			t.Fatalf("archive %s: %v", target, err)
		}
		context := filepath.Join(root, "images", target.OS+"-"+target.Arch)
		if target.OS == "darwin" {
			if _, err := os.Lstat(context); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unselected Darwin context exists: %v", err)
			}
			continue
		}
		binary, err := os.ReadFile(filepath.Join(context, target.binaryName()))
		if err != nil || !bytes.Equal(binary, archived.Data) {
			t.Fatalf("image %s differs from its same-build archive: %v", target, err)
		}
		if target.OS == "windows" {
			assertWindowsImageBinary(t, filepath.Join(context, imageOperatorName(target)))
		} else {
			assertImageBinaryBuildInputs(t, filepath.Join(context, imageOperatorName(target)), target.Arch)
		}
	}
	assertNoImageStaging(t, root)
}

func TestReleaseImageSubsetUsesOnlySelectedNativePlatform(t *testing.T) {
	useImageTestBinaries(t)
	engine := useFakeImageEngine(t, Target{"linux", "arm64"})
	// A Windows archive does not need Windows image inputs when unselected.
	original := windowsImageSourceDirectory
	windowsImageSourceDirectory = filepath.Join(t.TempDir(), "absent-windows-inputs")
	t.Cleanup(func() { windowsImageSourceDirectory = original })
	root := t.TempDir()
	args := append(imageTestArgs(root), "-targets=", "-image-targets", "linux/arm64", "-build-images", "-image-prefix", "subset-test")
	if err := run(args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(engine.images) != 3 {
		t.Fatalf("selected native Linux target must build exactly three families: %v", engine.images)
	}
	for _, target := range DefaultTargets {
		if _, err := os.Stat(filepath.Join(root, "assets", target.archiveName("v0.4.0-rc.1"))); err != nil {
			t.Fatalf("archive target %s was omitted: %v", target, err)
		}
	}
	assertNoImageStaging(t, root)
}

func TestReleaseImageSubsetFailureKeepsBatchHidden(t *testing.T) {
	useImageTestBinaries(t)
	root := t.TempDir()
	args := append(imageTestArgs(root), "-targets", "linux/arm64,invalid/arch", "-image-targets", "linux/arm64")
	if err := run(args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "build invalid/arch failed") {
		t.Fatalf("expected later archive failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "assets", "goobers_v0.4.0-rc.1_linux_arm64.tar.gz")); err != nil {
		t.Fatalf("selected archive must succeed before the later failure: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "images")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later archive failure exposed selected contexts: %v", err)
	}
	assertNoImageStaging(t, root)
}
