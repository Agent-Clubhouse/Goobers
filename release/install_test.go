package main

import (
	"bytes"
	"os"
	"runtime"
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
	if runtime.GOOS == "windows" {
		// Windows has no unix-style executable bit — os.WriteFile's 0o755
		// mode argument is honored request-side (writeInstallScript still
		// asks for it, correctly, since a POSIX install target needs it) but
		// the filesystem always reports a plain writable file back. Asserting
		// 0o755 here would be asserting a platform Windows cannot represent,
		// not a real regression.
		return
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installer mode = %o, want 755", info.Mode().Perm())
	}
}
