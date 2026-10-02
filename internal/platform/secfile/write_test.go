package secfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePrivate_CreatesVerifiablyPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := WritePrivate(path, []byte("secret")); err != nil {
		t.Fatalf("WritePrivate: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret" {
		t.Fatalf("content = %q, want %q", got, "secret")
	}
	if err := VerifyPrivate(path); err != nil {
		t.Fatalf("VerifyPrivate after WritePrivate: %v", err)
	}
}

// A pre-existing, broadly readable file must be narrowed and fully replaced —
// the create mode / creation-time descriptor alone would not touch it.
func TestWritePrivate_NarrowsAndTruncatesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("a much longer previous secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeBroadlyReadable(t, path)
	if err := VerifyPrivate(path); err == nil {
		t.Fatal("fixture should start non-private")
	}
	if err := WritePrivate(path, []byte("new")); err != nil {
		t.Fatalf("WritePrivate: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
	if err := VerifyPrivate(path); err != nil {
		t.Fatalf("VerifyPrivate after WritePrivate: %v", err)
	}
}

func TestWritePrivate_FailsOnMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "credentials.json")
	if err := WritePrivate(path, []byte("secret")); err == nil {
		t.Fatal("WritePrivate into a missing directory succeeded")
	}
}
