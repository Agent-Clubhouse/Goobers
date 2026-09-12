package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePortalSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"index.html":              "<!doctype html><script src=/assets/app-abc123.js></script>",
		"assets/app-abc123.js":    "console.log('portal')",
		"assets/style-def456.css": "body{}",
	}
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPortalArchiveCarriesEveryAsset(t *testing.T) {
	root := writePortalSource(t)
	out := t.TempDir()
	archive, err := packagePortalAssets(root, "v0.4.0-rc.3", out)
	if err != nil {
		t.Fatalf("packagePortalAssets: %v", err)
	}

	names := map[string]string{}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err != nil {
			break
		}
		buf, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read %s: %v", header.Name, err)
		}
		names[header.Name] = string(buf)
	}
	for _, want := range []string{"index.html", "assets/app-abc123.js", "assets/style-def456.css"} {
		if _, ok := names[want]; !ok {
			t.Errorf("portal archive is missing %s; carries %v", want, names)
		}
	}
}

// The verification must be capable of failing, or it is decoration. These
// drive verifyPortalArchive directly with an entry set the archive does not
// satisfy — the shapes a packaging bug would actually produce.
func TestPortalArchiveVerificationDeniesADefectiveArchive(t *testing.T) {
	root := writePortalSource(t)
	out := t.TempDir()
	archive, err := packagePortalAssets(root, "v0.4.0-rc.3", out)
	if err != nil {
		t.Fatalf("packagePortalAssets: %v", err)
	}
	packed, err := collectArchiveEntries(root)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("missing file", func(t *testing.T) {
		want := append(append([]archiveEntry{}, packed...),
			archiveEntry{name: "assets/dropped-999.js", mode: 0o644, data: []byte("x")})
		err := verifyPortalArchive(archive, want)
		if err == nil || !strings.Contains(err.Error(), "missing assets/dropped-999.js") {
			t.Errorf("verifyPortalArchive() = %v, want a missing-file refusal", err)
		}
	})

	t.Run("different contents", func(t *testing.T) {
		want := append([]archiveEntry{}, packed...)
		for i := range want {
			if want[i].name == "index.html" {
				want[i].data = []byte("<!doctype html>stale")
			}
		}
		err := verifyPortalArchive(archive, want)
		if err == nil || !strings.Contains(err.Error(), "different contents") {
			t.Errorf("verifyPortalArchive() = %v, want a contents mismatch refusal", err)
		}
	})

	t.Run("extra file in archive", func(t *testing.T) {
		var want []archiveEntry
		for _, entry := range packed {
			if entry.name != "assets/style-def456.css" {
				want = append(want, entry)
			}
		}
		err := verifyPortalArchive(archive, want)
		if err == nil || !strings.Contains(err.Error(), "carries 3 file(s)") {
			t.Errorf("verifyPortalArchive() = %v, want a file-count refusal", err)
		}
	})
}

// A portal without index.html is not a portal, however many hashed assets it
// carries — and index.html is precisely the one file the old smoke check
// looked at, so an archive that kept it and lost everything else also passed.
func TestPortalArchiveRequiresIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app-abc123.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := packagePortalAssets(root, "v0.4.0-rc.3", t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "no index.html") {
		t.Errorf("packagePortalAssets() = %v, want an index.html refusal", err)
	}
}
