package configgeneration

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/gooberassets"
)

func TestCapturedAssetIdentitySurvivesRefetchAndRecovery(t *testing.T) {
	source := t.TempDir()
	assets := filepath.Join(source, "config", "goobers", "coder", "assets")
	writeFixture(t, assets, "script.sh", "echo pinned\n")
	writeFixture(t, assets, "a/child", "nested")
	writeFixture(t, assets, "a-file", "prefix sibling")
	writeFixture(t, assets, ".git/payload", "operator asset")
	writeFixture(t, assets, "nested/.git", "operator asset file")
	if err := os.Chmod(filepath.Join(assets, "script.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	original, err := gooberassets.Load(assets)
	if err != nil {
		t.Fatal(err)
	}
	data, digest, err := CaptureForInstance(t.Context(), filepath.Join(source, "config"), "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	again, repeated, err := CaptureForInstance(t.Context(), filepath.Join(source, "config"), "instance-1")
	if err != nil || digest != repeated || !bytes.Equal(data, again) {
		t.Fatal("synthesized metadata is nondeterministic")
	}
	if _, err := os.Stat(assets + gooberassets.SourceModeSuffix); !os.IsNotExist(err) {
		t.Fatal("capture mutated operator-owned source")
	}
	blobs, err := blobstore.NewDir(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	admitted := Store{Root: filepath.Join(t.TempDir(), "admitted"), Blobs: blobs}
	if _, err := admitted.Keep(t.Context(), data, digest, nil); err != nil {
		t.Fatal(err)
	}
	fetched, err := blobs.GetBounded(t.Context(), digest, MaxArchiveBytes)
	if err != nil {
		t.Fatal(err)
	}
	worker := Store{Root: filepath.Join(t.TempDir(), "worker"), LocalCache: true}
	directory, err := worker.Keep(t.Context(), fetched, digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	recovered := Store{Root: worker.Root, LocalCache: true}
	if _, err := recovered.Load(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	bundle, err := gooberassets.Load(filepath.Join(directory, "goobers", "coder", "assets"))
	if err != nil || bundle.Fingerprint() != original.Fingerprint() {
		t.Fatalf("asset identity changed after refetch/recovery: %v", err)
	}
	validMetadata, err := original.SourceModeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, source, "config/goobers/coder/assets.goobers-source-modes.json", string(validMetadata))
	if _, _, err := CaptureForInstance(t.Context(), filepath.Join(source, "config"), "instance-1"); err != nil {
		t.Fatalf("valid mirrored source metadata refused: %v", err)
	}
	// Source metadata is itself checked, not trusted as an arbitrary claimed hash.
	writeFixture(t, source, "config/goobers/coder/assets.goobers-source-modes.json", `{"version":1,"modes":{},"fingerprint":"forged"}`)
	if _, _, err := CaptureForInstance(t.Context(), filepath.Join(source, "config"), "instance-1"); err == nil {
		t.Fatal("captured forged preexisting source modes")
	}
}

func TestUnixAuthoredAssetFingerprintSurvivesNativeExtraction(t *testing.T) {
	a := Archive{Version: 1, InstanceID: "unix-instance", Files: []file{
		{Path: "config", Mode: 0755, Directory: true},
		{Path: "config/goobers", Mode: 0755, Directory: true},
		{Path: "config/goobers/coder", Mode: 0755, Directory: true},
		{Path: "config/goobers/coder/assets", Mode: 0750, Directory: true},
		{Path: "config/goobers/coder/assets/run.sh", Mode: 0751, Data: []byte("echo source\n")},
		{Path: "config/goobers/coder/assets/a", Mode: 0750, Directory: true},
		{Path: "config/goobers/coder/assets/a/child", Mode: 0640, Data: []byte("nested")},
		{Path: "config/goobers/coder/assets/a-file", Mode: 0644, Data: []byte("prefix sibling")},
	}}
	expected := a.assetBundle(a.Files[3]).Fingerprint()
	data, digest, err := encodeCapturedArchive(a)
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Root: filepath.Join(t.TempDir(), "generations"), LocalCache: true}
	directory, err := store.Keep(t.Context(), data, digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDirectory(t.Context(), directory, digest, "unix-instance"); err != nil {
		t.Fatal(err)
	}
	actual, err := gooberassets.Load(filepath.Join(directory, "goobers", "coder", "assets"))
	if err != nil || actual.Fingerprint() != expected {
		t.Fatalf("Unix logical asset modes lost on native filesystem: %v", err)
	}
}

func TestSynthesizedAssetMetadataCountsAgainstArchiveLimits(t *testing.T) {
	base := []file{
		{Path: "config", Mode: 0755, Directory: true},
		{Path: "config/goobers", Mode: 0755, Directory: true},
		{Path: "config/goobers/coder", Mode: 0755, Directory: true},
		{Path: "config/goobers/coder/assets", Mode: 0755, Directory: true},
		{Path: "config/goobers/coder/assets/run.sh", Mode: 0755, Data: []byte("x")},
	}
	t.Run("files", func(t *testing.T) {
		a := Archive{Version: 1, Files: append([]file(nil), base...)}
		for len(a.Files) < MaxFiles {
			a.Files = append(a.Files, file{Path: fmt.Sprintf("config/filler-%d", len(a.Files)), Mode: 0600})
		}
		if err := a.includeAssetSourceModes(); err == nil {
			t.Fatal("source metadata bypassed file limit")
		}
	})
	t.Run("bytes", func(t *testing.T) {
		a := Archive{Version: 1, Files: append([]file(nil), base...)}
		a.Files = append(a.Files, file{Path: "config/filler", Mode: 0600, Data: make([]byte, MaxContentBytes-1)})
		if err := a.includeAssetSourceModes(); err == nil {
			t.Fatal("source metadata bypassed byte limit")
		}
	})
}
