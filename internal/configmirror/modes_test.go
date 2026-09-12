package configmirror

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/gooberassets"
)

func TestUnixArchiveAssetIdentitySurvivesNativeExtraction(t *testing.T) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for _, entry := range []struct {
		name string
		mode fs.FileMode
		body string
	}{
		{"instance.yaml", 0o640, "instance"},
		{"config/", fs.ModeDir | 0o750, ""},
		{"config/goobers/test/assets/", fs.ModeDir | 0o750, ""},
		{"config/goobers/test/assets/run.sh", 0o751, "script"},
	} {
		writer, err := archive.CreateHeader(snapshotHeader(entry.name, entry.mode))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	mirror := t.TempDir()
	if err := os.WriteFile(filepath.Join(mirror, SnapshotName), buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Open(mirror)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	destination := t.TempDir()
	if err := snapshot.Extract(t.Context(), destination); err != nil {
		t.Fatal(err)
	}
	actual, err := gooberassets.Load(filepath.Join(destination, "config/goobers/test/assets"))
	if err != nil {
		t.Fatal(err)
	}
	expected := gooberassets.FromWire(&gooberassets.WireBundle{
		RootMode: fs.ModeDir | 0o750,
		Entries:  []gooberassets.WireEntry{{Path: "run.sh", Mode: 0o751, Data: []byte("script")}},
	})
	if actual.Fingerprint() != expected.Fingerprint() {
		t.Fatalf("archive identity lost: %s != %s", actual.Fingerprint(), expected.Fingerprint())
	}
}

func TestSnapshotPreservesAssetFingerprint(t *testing.T) {
	config, mirror := t.TempDir(), t.TempDir()
	assets := filepath.Join(config, "goobers/test/assets")
	script := filepath.Join(assets, "scripts/run.sh")
	writeTestFile(t, script, "#!/bin/sh\necho hello\n")
	if err := os.Chmod(script, 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(assets, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	before, err := gooberassets.Load(assets)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(t.Context(), mirror, config, []byte("instance")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Open(mirror)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	destination := t.TempDir()
	if err := snapshot.Extract(t.Context(), destination); err != nil {
		t.Fatal(err)
	}
	after, err := gooberassets.Load(filepath.Join(destination, "config/goobers/test/assets"))
	if err != nil {
		t.Fatal(err)
	}
	if before.Fingerprint() != after.Fingerprint() {
		t.Fatalf("asset identity changed: %s != %s", before.Fingerprint(), after.Fingerprint())
	}
}

func TestSnapshotRejectsCaseAliasedDirectories(t *testing.T) {
	seen := make(map[string]bool)
	if err := reserveSnapshotName(seen, "config/A/x"); err != nil {
		t.Fatal(err)
	}
	if err := reserveSnapshotName(seen, "config/a/y"); err == nil {
		t.Fatal("accepted directory aliases that collide on Windows")
	}
}
