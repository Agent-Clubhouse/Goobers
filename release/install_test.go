package main

import (
	"bytes"
	"os"
	"testing"
)

func TestWriteInstallScript(t *testing.T) {
	path, err := writeInstallScript(t.TempDir())
	if err != nil {
		t.Fatalf("writeInstallScript: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(installScript)) {
		t.Fatal("written installer differs from embedded installer")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installer mode = %o, want 755", info.Mode().Perm())
	}
}
