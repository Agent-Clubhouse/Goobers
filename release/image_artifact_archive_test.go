package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func finalArchiveFixture(t *testing.T, target Target, data []byte) string {
	t.Helper()
	directory := t.TempDir()
	name := target.archiveName("v0.4.0-rc.1")
	if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	return directory
}

func finalArchiveBytes(t *testing.T, target Target, entries []archiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var err error
	if target.OS == "windows" {
		err = writeZip(&buffer, entries)
	} else {
		err = writeTarGz(&buffer, entries)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestFinalArchiveBinaryPreservesFinalSignedBytesAndProvenance(t *testing.T) {
	for _, target := range []Target{{"linux", "amd64"}, {"windows", "amd64"}} {
		t.Run(target.OS, func(t *testing.T) {
			// Treat a signing overlay as opaque bytes. Import must not rebuild, parse,
			// normalize, or truncate the payload after a PE certificate-table boundary.
			payload := append([]byte("MZ\x00opaque executable\x00"), bytes.Repeat([]byte{0xff, 0x00, 0x80, 0x42}, 257)...)
			data := finalArchiveBytes(t, target, []archiveEntry{{name: target.binaryName(), mode: 0755, data: payload}, {name: "docs/readme.md", mode: 0644, data: []byte("documentation")}})
			directory := finalArchiveFixture(t, target, data)
			got, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Data, payload) || got.ArchiveName != target.archiveName("v0.4.0-rc.1") || got.ArchiveSHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
				t.Fatalf("final artifact bytes or provenance changed: %+v", got)
			}
		})
	}
}

func TestFinalArchiveRejectsUnsafeOrAmbiguousEntries(t *testing.T) {
	for _, target := range []Target{{"linux", "amd64"}, {"windows", "amd64"}} {
		for _, tc := range []struct {
			name    string
			entries []archiveEntry
		}{
			{"missing", []archiveEntry{{name: "nested/" + target.binaryName(), mode: 0755, data: []byte("nested")}}},
			{"duplicate", []archiveEntry{{name: target.binaryName(), mode: 0755, data: []byte("second")}}},
			{"case collision", []archiveEntry{{name: strings.ToUpper(target.binaryName()), mode: 0755, data: []byte("second")}}},
			{"traversal", []archiveEntry{{name: "../outside", mode: 0644, data: []byte("escape")}}},
			{"absolute", []archiveEntry{{name: "/outside", mode: 0644, data: []byte("escape")}}},
			{"backslash", []archiveEntry{{name: "docs\\outside", mode: 0644, data: []byte("escape")}}},
			{"DOS device", []archiveEntry{{name: "docs/CON.txt", mode: 0644, data: []byte("device")}}},
			{"alternate stream", []archiveEntry{{name: "docs:outside", mode: 0644, data: []byte("escape")}}},
			{"file parent", []archiveEntry{{name: "docs", mode: 0644, data: []byte("file")}, {name: "docs/readme", mode: 0644, data: []byte("nested")}}},
			{"parent file after child", []archiveEntry{{name: "docs/readme", mode: 0644, data: []byte("nested")}, {name: "docs", mode: 0644, data: []byte("file")}}},
			{"nonexecutable", []archiveEntry{{name: target.binaryName(), mode: 0644, data: []byte("binary")}}},
		} {
			t.Run(target.OS+"/"+tc.name, func(t *testing.T) {
				entries := tc.entries
				if tc.name != "missing" && tc.name != "nonexecutable" {
					entries = append([]archiveEntry{{name: target.binaryName(), mode: 0755, data: []byte("binary")}}, entries...)
				}
				directory := finalArchiveFixture(t, target, finalArchiveBytes(t, target, entries))
				if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target); err == nil {
					t.Fatal("unsafe archive accepted")
				}
			})
		}
	}
}

func TestFinalArchiveChecksumIsVerifiedBeforeParsing(t *testing.T) {
	target := Target{"linux", "amd64"}
	directory := finalArchiveFixture(t, target, []byte("not a valid archive"))
	name := target.archiveName("v0.4.0-rc.1")
	if err := os.WriteFile(filepath.Join(directory, name), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target); err == nil || !strings.Contains(err.Error(), "SHA256 mismatch") {
		t.Fatalf("archive parsed before checksum validation: %v", err)
	}
}

func TestImageArtifactManifestRejectsMalformedOrAmbiguousEntries(t *testing.T) {
	digest := strings.Repeat("a", 64)
	valid := digest + "  release.zip\n"
	for _, manifest := range []string{"", "\n", digest + " release.zip\n", "g" + digest[1:] + "  release.zip\n", valid + valid, valid + digest + "  RELEASE.ZIP\n", valid + "\n", digest + "  ../release.zip\n", digest + "  /release.zip\n", digest + "  dir\\release.zip\n", digest + "  dir:release.zip\n", digest + "  release.zip \n", digest + "  SHA256SUMS\n", strings.Repeat("a", int(imageArtifactManifestLimit)+1)} {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readImageArtifactManifest(directory); err == nil {
			t.Fatalf("invalid manifest accepted (length %d)", len(manifest))
		}
	}
}

func TestImageArtifactHelpersRequireExactBoundedRegularFiles(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("pinned bytes")
	if err := os.WriteFile(filepath.Join(directory, "nested", "input"), data, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("%X  nested/input\n", sha256.Sum256(data))
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	sums, err := readImageArtifactManifest(directory)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readVerifiedImageArtifact(directory, "nested/input", sums, 1024)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("verified input failed: %q %v", got, err)
	}
	for _, name := range []string{"nested/Input", "../nested/input", "nested"} {
		if _, err := readVerifiedImageArtifact(directory, name, sums, 1024); err == nil {
			t.Fatalf("nonexact input %q accepted", name)
		}
	}
	if _, err := readVerifiedImageArtifact(directory, "nested/input", sums, int64(len(data)-1)); err == nil {
		t.Fatal("oversize input accepted")
	}
	if _, err := readRegularImageArtifact(directory, "nested", 1024); err == nil {
		t.Fatal("directory accepted as regular artifact")
	}
}

func TestFinalArchiveZIPDOSExecutableAndCorruptNonbinaryPayload(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range []string{"goobers.exe", "docs/readme"} {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		// No SetMode: the standard DOS-origin header has no POSIX execute bits.
		body, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := body.Write([]byte("recognizable payload " + name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	target := Target{"windows", "amd64"}
	data := buffer.Bytes()
	directory := finalArchiveFixture(t, target, data)
	if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target); err != nil {
		t.Fatalf("DOS .exe refused: %v", err)
	}
	corrupt := bytes.Clone(data)
	position := bytes.Index(corrupt, []byte("recognizable payload docs/readme"))
	if position < 0 {
		t.Fatal("missing uncompressed fixture marker")
	}
	corrupt[position] ^= 1
	directory = finalArchiveFixture(t, target, corrupt)
	if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target); err == nil {
		t.Fatal("corrupt nonbinary ZIP payload ignored")
	}
}

func TestFinalArchiveRejectsLinksAndDevices(t *testing.T) {
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		var buffer bytes.Buffer
		gz := gzip.NewWriter(&buffer)
		writer := tar.NewWriter(gz)
		if err := writer.WriteHeader(&tar.Header{Name: "goobers", Mode: 0755, Size: 3}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("bin")); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteHeader(&tar.Header{Name: "unsafe", Typeflag: kind, Linkname: "goobers", Mode: 0755}); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		target := Target{"linux", "amd64"}
		directory := finalArchiveFixture(t, target, buffer.Bytes())
		if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target); err == nil {
			t.Fatalf("tar entry type %d accepted", kind)
		}
	}
	for _, mode := range []os.FileMode{os.ModeSymlink, os.ModeDevice, os.ModeNamedPipe, os.ModeSetuid} {
		var buffer bytes.Buffer
		writer := zip.NewWriter(&buffer)
		header := &zip.FileHeader{Name: "goobers.exe"}
		header.SetMode(mode | 0755)
		body, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := body.Write([]byte("bin")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		target := Target{"windows", "amd64"}
		directory := finalArchiveFixture(t, target, buffer.Bytes())
		if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target); err == nil {
			t.Fatalf("ZIP special mode %v accepted", mode)
		}
	}
}

func TestFinalArchiveRejectsMalformedTrailersAndExpansionBombHeaders(t *testing.T) {
	target := Target{"linux", "amd64"}
	good := finalArchiveBytes(t, target, []archiveEntry{{name: "goobers", mode: 0755, data: []byte("binary")}})
	corrupt := bytes.Clone(good)
	corrupt[len(corrupt)-8] ^= 1
	for _, data := range [][]byte{[]byte("malformed"), good[:len(good)-1], corrupt, append(bytes.Clone(good), good...), append(bytes.Clone(good), 0)} {
		directory := finalArchiveFixture(t, target, data)
		if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target); err == nil {
			t.Fatal("malformed gzip trailer accepted")
		}
	}
	windows := Target{"windows", "amd64"}
	good = finalArchiveBytes(t, windows, []archiveEntry{{name: "goobers.exe", mode: 0755, data: []byte("binary")}})
	bomb := bytes.Clone(good)
	central := bytes.Index(bomb, []byte("PK\x01\x02"))
	if central < 0 {
		t.Fatal("missing ZIP central directory")
	}
	binary.LittleEndian.PutUint32(bomb[central+24:central+28], uint32(imageArtifactExpandedLimit+1))
	for _, data := range [][]byte{good[:len(good)-1], append(bytes.Clone(good), 0), bomb} {
		directory := finalArchiveFixture(t, windows, data)
		if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", windows); err == nil {
			t.Fatal("malformed or oversized ZIP accepted")
		}
	}
	var entries imageArchiveEntries
	if err := entries.accept("large", 0644, imageArtifactExpandedLimit); err != nil {
		t.Fatal(err)
	}
	if err := entries.accept("overflow", 0644, 1); err == nil {
		t.Fatal("expanded aggregate limit ignored")
	}
	if err := entries.readBody(strings.NewReader(""), "goobers", "goobers", imageArtifactBinaryLimit+1, true); err == nil {
		t.Fatal("binary size limit ignored")
	}
	entries = imageArchiveEntries{count: imageArtifactEntryLimit}
	if err := entries.accept("overflow", 0644, 0); err == nil {
		t.Fatal("entry count limit ignored")
	}
}

func TestFinalArchiveRefusesOversizedCompressedInputBeforeReading(t *testing.T) {
	directory := t.TempDir()
	target := Target{"linux", "amd64"}
	name := target.archiveName("v0.4.0-rc.1")
	file, err := os.Create(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(imageArtifactArchiveLimit + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := strings.Repeat("a", 64) + "  " + name + "\n"
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFinalArchiveBinary(directory, "v0.4.0-rc.1", target); err == nil || !strings.Contains(err.Error(), "bounded regular") {
		t.Fatalf("oversized compressed archive was read: %v", err)
	}
}
