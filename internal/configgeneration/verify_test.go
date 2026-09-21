package configgeneration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// This fixture is explicitly Unix-authored even on Windows; capturing the host
// filesystem would conceal NTFS's inability to preserve the source mode bits.
func unixGenerationFixture(t *testing.T) (Store, string, string) {
	t.Helper()
	archive := Archive{Version: 1, InstanceID: "source-instance", Files: []file{
		{Path: "config", Mode: 0755, Directory: true},
		{Path: "config/run.sh", Mode: 0755, Data: []byte("echo original\n")},
		{Path: "goobers", Mode: 0755, Directory: true},
		{Path: "goobers/instructions.md", Mode: 0640, Data: []byte("original instructions\n")},
	}}
	data, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	store := Store{Root: filepath.Join(t.TempDir(), "generations"), LocalCache: true}
	directory, err := store.Keep(t.Context(), data, digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, directory, digest
}

func TestUnixSourceGenerationKeepsIdentityAcrossNativeExtraction(t *testing.T) {
	store, directory, digest := unixGenerationFixture(t)
	if _, err := store.Load(t.Context(), digest); err != nil {
		t.Fatalf("native extraction cannot load Unix source: %v", err)
	}
	if err := VerifyDirectory(t.Context(), directory, digest, "source-instance"); err != nil {
		t.Fatalf("CLI verification lost original generation: %v", err)
	}
	_, recaptured, err := CaptureForInstance(t.Context(), directory, "source-instance")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && recaptured == digest {
		t.Fatal("fixture failed to distinguish original Unix modes from Windows permissions")
	}
	if err := VerifyDirectory(t.Context(), directory, digest, "another-instance"); err == nil {
		t.Fatal("cross-instance archive accepted")
	}
}

func TestManifestVerificationRejectsChangedTree(t *testing.T) {
	mutations := map[string]func(string) error{
		"changed bytes":       func(dir string) error { return os.WriteFile(filepath.Join(dir, "run.sh"), []byte("changed"), 0600) },
		"missing file":        func(dir string) error { return os.Remove(filepath.Join(dir, "run.sh")) },
		"added file":          func(dir string) error { return os.WriteFile(filepath.Join(dir, "extra"), nil, 0600) },
		"added git directory": func(dir string) error { return os.Mkdir(filepath.Join(dir, ".git"), 0700) },
		"file becomes directory": func(dir string) error {
			if err := os.Remove(filepath.Join(dir, "run.sh")); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(dir, "run.sh"), 0700)
		},
		"changed writable permission": func(dir string) error { return os.Chmod(filepath.Join(dir, "run.sh"), 0400) },
		"symlink replaces file": func(dir string) error {
			if err := os.Remove(filepath.Join(dir, "run.sh")); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(filepath.Dir(dir), "goobers", "instructions.md"), filepath.Join(dir, "run.sh"))
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			store, directory, digest := unixGenerationFixture(t)
			if err := mutate(directory); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(t.Context(), digest); err == nil {
				t.Fatal("recovery accepted mutated generation")
			}
			if err := VerifyDirectory(t.Context(), directory, digest, "source-instance"); err == nil {
				t.Fatal("CLI accepted mutated generation")
			}
			// Windows cannot remove read-only files during TempDir cleanup.
			_ = os.Chmod(filepath.Join(directory, "run.sh"), 0600)
		})
	}
}
