package secfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePrivateAtomicReplacesExposedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte("old longer content"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeBroadlyReadable(t, path)
	if err := VerifyPrivate(path); err == nil {
		t.Fatal("fixture must start exposed")
	}
	if err := WritePrivateAtomic(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPrivate(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "secret" {
		t.Fatalf("content=%q error=%v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files retained: %v, error=%v", entries, err)
	}
}

func TestWritePrivateAtomicFailedPublicationCleansSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "occupied")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "original")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateAtomic(path, []byte("secret")); err == nil {
		t.Fatal("replaced a nonempty directory")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "keep" {
		t.Fatalf("original destination changed: %q, %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "occupied" {
		t.Fatalf("temporary secret retained: %v, error=%v", entries, err)
	}
}
