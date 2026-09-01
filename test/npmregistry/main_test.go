package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyAcceptsPublicRegistry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeLockfile(t, root, "portal", `"https://registry.npmjs.org/three/-/three-0.185.1.tgz"`)
	if err := verify(root); err != nil {
		t.Fatalf("verify public registry lockfile: %v", err)
	}
}

func TestVerifyRejectsPrivateMirror(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeLockfile(t, root, "portal", `"https://ms-feed-12.pkgs.visualstudio.com/1es-public/_packaging/npm-public/npm/registry/three/-/three-0.185.1.tgz"`)
	err := verify(root)
	if err == nil {
		t.Fatal("verify accepted a private mirror tarball URL")
	}
	if !strings.Contains(err.Error(), "ms-feed-12.pkgs.visualstudio.com") {
		t.Fatalf("error does not name the offending host: %v", err)
	}
}

func TestVerifyIgnoresEntriesWithoutARemoteTarball(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeLockfile(t, root, "portal", `"../local-package"`)
	if err := verify(root); err != nil {
		t.Fatalf("verify link entry lockfile: %v", err)
	}
}

func TestVerifySkipsVendoredLockfiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeLockfile(t, root, filepath.Join("portal", "node_modules", "three"),
		`"https://ms-feed-12.pkgs.visualstudio.com/1es-public/_packaging/npm-public/npm/registry/three/-/three-0.185.1.tgz"`)
	if err := verify(root); err != nil {
		t.Fatalf("verify ignored an installed dependency's own lockfile: %v", err)
	}
}

func writeLockfile(t *testing.T, root, dir, resolved string) {
	t.Helper()
	path := filepath.Join(root, dir, lockfileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create lockfile directory: %v", err)
	}
	body := `{"packages":{"node_modules/three":{"version":"0.185.1","resolved":` + resolved + `}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
}
