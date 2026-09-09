package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func importTestArgs(root, targets string) []string {
	return []string{"-version", "v0.4.0-rc.1", "-commit", "0123456789ab", "-date", "2026-09-07T12:00:00Z", "-targets", targets, "-image-artifacts", filepath.Join(root, "assets"), "-image-inputs", filepath.Join(root, "original"), "-image-contexts", filepath.Join(root, "imported")}
}

func TestImageImportFlagsRequireExplicitIdentityAndRefuseReleaseMutation(t *testing.T) {
	base := importTestArgs(t.TempDir(), "linux/arm64")
	for _, missing := range []string{"-version", "-commit", "-date", "-targets", "-image-artifacts", "-image-inputs", "-image-contexts"} {
		t.Run(missing, func(t *testing.T) {
			var args []string
			for i := 0; i < len(base); i += 2 {
				if base[i] != missing {
					args = append(args, base[i:i+2]...)
				}
			}
			if _, err := parseFlags(args, io.Discard); err == nil {
				t.Fatal("incomplete import admitted")
			}
		})
	}
	for _, extra := range []string{"-output=unused", "-checksums=false", "-first-feature-snapshot", "-previous-features=old.json", "-previous-support-matrix=old.json", "-skip-unbuildable", "-date=unknown", "-commit=dirty"} {
		t.Run(extra, func(t *testing.T) {
			if _, err := parseFlags(append(append([]string(nil), base...), extra), io.Discard); err == nil {
				t.Fatal("inapplicable import option admitted")
			}
		})
	}
}

func prepareImportFixture(t *testing.T, root string, target Target) []byte {
	t.Helper()
	opts, err := parseFlags(importTestArgs(root, target.String()), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(opts.imageInputs, target.OS+"-"+target.Arch)
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	repo, err := findAgentToolkitRepositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := os.ReadFile(filepath.Join(repo, "packaging", "docker", "base", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"Dockerfile": recipe, target.binaryName(): []byte("original CLI"), imageOperatorName(target): []byte("original operator")}
	if target.OS == "windows" {
		materials, err := readWindowsImageMaterials(repo)
		if err != nil {
			t.Fatal(err)
		}
		for name, data := range materials {
			files[name] = data
		}
		if err := prepareWindowsImageDependencies(directory, materials["dependencies.json"]); err != nil {
			t.Fatal(err)
		}
	}
	metadata := imageContextMetadata{1, "goobers-base-build-inputs", opts.version, opts.commit, opts.date, target.String()}
	if err := writeImageContextFiles(directory, files, metadata); err != nil {
		t.Fatal(err)
	}
	final := []byte("final CLI bytes including an Authenticode-like overlay\x00\xff")
	writeImportTestArchive(t, opts.imageArtifacts, opts.version, target, final)
	return final
}

func writeImportTestArchive(t *testing.T, directory, version string, target Target, data []byte) {
	t.Helper()
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if target.OS == "windows" {
		writer := zip.NewWriter(&archive)
		entry, err := writer.Create(target.binaryName())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed := gzip.NewWriter(&archive)
		writer := tar.NewWriter(compressed)
		if err := writer.WriteHeader(&tar.Header{Name: target.binaryName(), Mode: 0755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(directory, target.archiveName(version))
	if err := os.WriteFile(path, archive.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	manifest, err := checksumsManifest([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestImageImportPreservesFinalBytesWithoutBuildOrSourceMutation(t *testing.T) {
	useWindowsImageFixtures(t, false)
	oldBuild, oldOperator, oldPortal := buildPackage, operatorBuildPackage, portalAssetsDirectory
	buildPackage, operatorBuildPackage, portalAssetsDirectory = "./missing-cli", "./missing-operator", "missing-portal"
	t.Cleanup(func() { buildPackage, operatorBuildPackage, portalAssetsDirectory = oldBuild, oldOperator, oldPortal })
	for _, target := range []Target{{OS: "linux", Arch: "arm64"}, {OS: "windows", Arch: "amd64"}} {
		t.Run(target.String(), func(t *testing.T) {
			root := t.TempDir()
			final := prepareImportFixture(t, root, target)
			original := filepath.Join(root, "original", target.OS+"-"+target.Arch)
			before, err := os.ReadFile(filepath.Join(original, "SHA256SUMS"))
			if err != nil {
				t.Fatal(err)
			}
			if err := run(importTestArgs(root, target.String()), io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			imported := filepath.Join(root, "imported", target.OS+"-"+target.Arch)
			for name, want := range map[string][]byte{target.binaryName(): final, imageOperatorName(target): []byte("original operator")} {
				got, err := os.ReadFile(filepath.Join(imported, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s bytes changed: %v", name, err)
				}
			}
			after, err := os.ReadFile(filepath.Join(original, "SHA256SUMS"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("source context mutated")
			}
			unchanged, err := os.ReadFile(filepath.Join(original, target.binaryName()))
			if err != nil || string(unchanged) != "original CLI" {
				t.Fatal("source CLI mutated")
			}
			proof, err := os.ReadFile(filepath.Join(imported, "artifact-import.json"))
			if err != nil {
				t.Fatal(err)
			}
			var provenance imageArtifactImportEvidence
			if err := json.Unmarshal(proof, &provenance); err != nil {
				t.Fatal(err)
			}
			if provenance.ArchiveName != target.archiveName("v0.4.0-rc.1") || provenance.ArchiveSHA256 == "" || len(provenance.OriginalContextChecksums) == 0 {
				t.Fatalf("missing import evidence: %+v", provenance)
			}
			manifest, err := readImageArtifactManifest(imported)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readVerifiedImageArtifact(imported, "artifact-import.json", manifest, 1<<20); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImageImportRejectsSkewAndModifiedInputsAtomically(t *testing.T) {
	for _, which := range []string{"release.json", "Dockerfile", "goobers-operator", "extra"} {
		t.Run(which, func(t *testing.T) {
			root := t.TempDir()
			target := Target{OS: "linux", Arch: "arm64"}
			prepareImportFixture(t, root, target)
			original := filepath.Join(root, "original", "linux-arm64")
			if err := os.WriteFile(filepath.Join(original, which), []byte("changed"), 0644); err != nil {
				t.Fatal(err)
			}
			if which != "goobers-operator" {
				if err := os.Remove(filepath.Join(original, "SHA256SUMS")); err != nil {
					t.Fatal(err)
				}
				if err := writeImageContextChecksums(original); err != nil {
					t.Fatal(err)
				}
			}
			if err := run(importTestArgs(root, target.String()), io.Discard, io.Discard); err == nil {
				t.Fatal("modified/skewed image inputs admitted")
			}
			if _, err := os.Stat(filepath.Join(root, "imported")); !os.IsNotExist(err) {
				t.Fatalf("failed batch finalized: %v", err)
			}
			assertNoImageStaging(t, root)
		})
	}
}

func TestImageImportRefusesDestinationInsideSource(t *testing.T) {
	root := t.TempDir()
	target := Target{OS: "linux", Arch: "arm64"}
	prepareImportFixture(t, root, target)
	args := append(importTestArgs(root, target.String()), "-image-contexts", filepath.Join(root, "original", "nested"))
	if err := run(args, io.Discard, io.Discard); err == nil {
		t.Fatal("input directory mutation permitted")
	}
}

func TestImageImportDiscardsEarlierPlatformWhenLaterInputFails(t *testing.T) {
	useWindowsImageFixtures(t, false)
	root := t.TempDir()
	targets := []Target{{OS: "linux", Arch: "arm64"}, {OS: "windows", Arch: "amd64"}}
	var archives []string
	for _, target := range targets {
		prepareImportFixture(t, root, target)
		archives = append(archives, filepath.Join(root, "assets", target.archiveName("v0.4.0-rc.1")))
	}
	manifest, err := checksumsManifest(archives)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "SHA256SUMS"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "original", "windows-amd64", "release.json"), []byte("wrong metadata"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run(importTestArgs(root, "linux/arm64,windows/amd64"), io.Discard, io.Discard); err == nil {
		t.Fatal("partially valid import succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "imported")); !os.IsNotExist(err) {
		t.Fatalf("partial import finalized: %v", err)
	}
	assertNoImageStaging(t, root)
}
