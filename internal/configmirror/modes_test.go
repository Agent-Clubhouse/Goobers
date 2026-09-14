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

// TestValidSnapshotModeAcceptsNormalizedSetgidAndStickyDirectories uses
// synthetic fs.FileMode values, so it needs no setgid-capable filesystem: it
// documents that normalizeDirMode must run before validSnapshotMode sees a
// directory's mode, since validSnapshotMode itself still refuses any bit
// beyond permissions and the directory flag.
func TestValidSnapshotModeAcceptsNormalizedSetgidAndStickyDirectories(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode fs.FileMode
	}{
		{"setgid directory", fs.ModeDir | fs.ModeSetgid | 0o750},
		{"sticky directory", fs.ModeDir | fs.ModeSticky | 0o750},
		{"setgid and sticky directory", fs.ModeDir | fs.ModeSetgid | fs.ModeSticky | 0o750},
	} {
		if validSnapshotMode(tc.mode) {
			t.Errorf("%s: validSnapshotMode accepted an unnormalized mode %v", tc.name, tc.mode)
		}
		if !validSnapshotMode(normalizeDirMode(tc.mode)) {
			t.Errorf("%s: validSnapshotMode rejected a normalized mode %v", tc.name, normalizeDirMode(tc.mode))
		}
	}
}

// TestPublishAcceptsSetgidDirectoriesAndFingerprintIsStable reproduces the bug
// reported from a Kubernetes pod where /tmp is an fsGroup-managed emptyDir:
// Kubernetes sets the directory setgid bit on such a volume, so every
// directory created beneath it (including every t.TempDir()) inherits
// fs.ModeSetgid, and local-ci's config mirror tests failed with "config mirror
// refuses non-regular file". The bit is set directly with os.Chmod rather than
// relying on the host filesystem to propagate it to children on mkdir, since
// that propagation is not guaranteed cross-platform (notably not on
// macOS/APFS), but a direct chmod is a faithful reproduction of what
// Kubernetes's kubelet does to an fsGroup volume and of what a shared-group
// Linux host does routinely.
func TestPublishAcceptsSetgidDirectoriesAndFingerprintIsStable(t *testing.T) {
	plainConfig, plainMirror := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(plainConfig, "goobers/test/assets/scripts/run.sh"), "#!/bin/sh\necho hello\n")
	if err := Publish(t.Context(), plainMirror, plainConfig, []byte("instance")); err != nil {
		t.Fatal(err)
	}
	plainSnapshot, err := Open(plainMirror)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plainSnapshot.Close() }()
	plainDestination := t.TempDir()
	if err := plainSnapshot.Extract(t.Context(), plainDestination); err != nil {
		t.Fatal(err)
	}
	plainAssets, err := gooberassets.Load(filepath.Join(plainDestination, "config/goobers/test/assets"))
	if err != nil {
		t.Fatal(err)
	}

	sgidConfig, sgidMirror := t.TempDir(), t.TempDir()
	assets := filepath.Join(sgidConfig, "goobers/test/assets")
	writeTestFile(t, filepath.Join(assets, "scripts/run.sh"), "#!/bin/sh\necho hello\n")
	for _, dir := range []string{assets, filepath.Join(assets, "scripts")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, info.Mode()|fs.ModeSetgid|fs.ModeSticky); err != nil {
			t.Skipf("cannot set directory setgid/sticky bits in this environment: %v", err)
		}
	}
	if err := Publish(t.Context(), sgidMirror, sgidConfig, []byte("instance")); err != nil {
		t.Fatalf("Publish refused a setgid/sticky config tree: %v", err)
	}
	sgidSnapshot, err := Open(sgidMirror)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sgidSnapshot.Close() }()
	sgidDestination := t.TempDir()
	if err := sgidSnapshot.Extract(t.Context(), sgidDestination); err != nil {
		t.Fatal(err)
	}
	sgidAssets, err := gooberassets.Load(filepath.Join(sgidDestination, "config/goobers/test/assets"))
	if err != nil {
		t.Fatal(err)
	}
	if sgidAssets.Fingerprint() != plainAssets.Fingerprint() {
		t.Fatalf("setgid config tree produced a different snapshot identity: %s != %s", sgidAssets.Fingerprint(), plainAssets.Fingerprint())
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
