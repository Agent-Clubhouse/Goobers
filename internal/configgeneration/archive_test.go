package configgeneration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCapturePreservesWholeGenerationAndExcludesInstanceSecrets(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "config/gaggles/example/workflows/implement.yaml", "kind: Workflow\n")
	writeFixture(t, root, "config/gaggles/example/goobers/coder/assets/.rules", "old instructions")
	writeFixture(t, root, "goobers/shared/instructions.md", "shared old instructions")
	writeFixture(t, root, "instance.yaml", "never copy this credential source")
	writeFixture(t, root, "config/.git/objects/unrelated", "do not snapshot git internals")
	data, digest, err := Capture(t.Context(), filepath.Join(root, "config"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "config/gaggles/example/goobers/coder/assets/.rules", "new instructions")
	archive, err := Decode(data, digest)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := archive.Extract(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"config/gaggles/example/goobers/coder/assets/.rules": "old instructions", "goobers/shared/instructions.md": "shared old instructions"} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s=%q err=%v", name, got, err)
		}
	}
	for _, name := range []string{"instance.yaml", "config/.git"} {
		if _, err := os.Stat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Fatalf("copied excluded %s: %v", name, err)
		}
	}
}

func TestCaptureHasStableIdentityAndPreservesEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "config", "gaggles", "example", "assets")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	first, digest, err := Capture(t.Context(), filepath.Join(root, "config"))
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := Capture(t.Context(), filepath.Join(root, "config"))
	if err != nil || again != digest {
		t.Fatalf("unstable capture: %s %s %v", digest, again, err)
	}
	archive, err := Decode(first, digest)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := archive.Extract(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dest, "config", "gaggles", "example", "assets")); err != nil || !info.IsDir() {
		t.Fatalf("empty directory not retained: %v", err)
	}
}

func TestDecodeRejectsUnsafeAndCorruptedArchives(t *testing.T) {
	for _, name := range []string{"../escape", "config/../../escape", "instance.yaml", "config\\escape", "config/file:stream", "config/NUL.yaml", "config/CON", "config/trailing.", "config/bad\nname"} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(Archive{Version: 1, Files: []file{{Path: name, Mode: 0600}}})
			sum := sha256.Sum256(data)
			if _, err := Decode(data, "sha256:"+hex.EncodeToString(sum[:])); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	if _, err := Decode([]byte(`{"version":1}`), "sha256:incorrect"); err == nil {
		t.Fatal("corrupt digest accepted")
	}
}

func TestCaptureRefusesOversizedAndCancelledGeneration(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "config/too-large", "x")
	if err := os.Truncate(filepath.Join(root, "config/too-large"), MaxFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Capture(t.Context(), filepath.Join(root, "config")); err == nil {
		t.Fatal("oversized file accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := Capture(ctx, filepath.Join(root, "config")); err == nil {
		t.Fatal("cancelled capture accepted")
	}
}

func TestDecodeRejectsCaseCollidingParents(t *testing.T) {
	data, _ := json.Marshal(Archive{Version: 1, Files: []file{{Path: "config/Assets/a", Mode: 0600}, {Path: "config/assets/b", Mode: 0600}}})
	sum := sha256.Sum256(data)
	if _, err := Decode(data, "sha256:"+hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("case-colliding parent paths accepted")
	}
}
