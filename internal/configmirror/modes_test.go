package configmirror

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/gooberassets"
)

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
