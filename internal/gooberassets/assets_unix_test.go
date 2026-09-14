//go:build unix

package gooberassets

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestLoadAndValidateRejectSpecialFile uses a FIFO — a unix-only special file —
// to prove the loader rejects a non-regular, non-directory asset. It lives in a
// unix-tagged file so the package's test build stays clean under
// GOOS=windows go vet (#1090's compile gate); the equivalent windows special
// file is out of scope until the Windows CI job lands.
func TestLoadAndValidateRejectSpecialFile(t *testing.T) {
	source := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(source, "stream"), 0o600); err != nil {
		t.Skipf("FIFO unsupported: %v", err)
	}
	if _, err := Load(source); err == nil {
		t.Fatal("Load accepted a FIFO asset")
	}
	if err := Validate(source); err == nil {
		t.Fatal("Validate accepted a FIFO asset")
	}
}

// TestLoadAcceptsSetgidDirectoriesAndFingerprintIsStable reproduces the bug
// reported from a Kubernetes pod where /tmp is an fsGroup-managed emptyDir:
// Kubernetes sets the directory setgid bit on such a volume, so every
// directory created beneath it (including every t.TempDir()) inherits
// fs.ModeSetgid. Before the fix, Load refused any such asset directory. This
// test sets the bit directly with os.Chmod rather than relying on the host
// filesystem to propagate it to children on mkdir, since that propagation is
// not guaranteed cross-platform (notably not on macOS/APFS), but a direct
// chmod is a faithful reproduction of what Kubernetes's kubelet does to an
// fsGroup volume and of what a shared-group Linux host does routinely.
func TestLoadAcceptsSetgidDirectoriesAndFingerprintIsStable(t *testing.T) {
	plain := t.TempDir()
	writeAsset(t, plain, "scripts/run.sh", "#!/bin/sh\necho hello\n", 0o751)
	baseline, err := Load(plain)
	if err != nil {
		t.Fatal(err)
	}

	setgid := t.TempDir()
	writeAsset(t, setgid, "scripts/run.sh", "#!/bin/sh\necho hello\n", 0o751)
	for _, dir := range []string{setgid, filepath.Join(setgid, "scripts")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, info.Mode()|fs.ModeSetgid|fs.ModeSticky); err != nil {
			t.Skipf("cannot set directory setgid/sticky bits in this environment: %v", err)
		}
	}
	loaded, err := Load(setgid)
	if err != nil {
		t.Fatalf("Load refused a setgid/sticky asset directory: %v", err)
	}
	if loaded.rootMode&(fs.ModeSetgid|fs.ModeSticky) != 0 {
		t.Fatalf("loaded root mode retained placement bits: %v", loaded.rootMode)
	}
	for _, e := range loaded.entries {
		if e.mode&(fs.ModeSetgid|fs.ModeSticky) != 0 {
			t.Fatalf("loaded entry %q retained placement bits: %v", e.path, e.mode)
		}
	}
	if loaded.Fingerprint() != baseline.Fingerprint() {
		t.Fatalf("setgid directory changed asset identity: %s != %s", loaded.Fingerprint(), baseline.Fingerprint())
	}
}
