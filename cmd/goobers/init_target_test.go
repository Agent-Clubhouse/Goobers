package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedSourceCheckoutCwd builds a directory shaped like a fresh clone of this
// repository — a tracked config/ holding CRD manifests plus a go.mod — and
// chdirs into it, reproducing the #2513 trap: `goobers init` with no [path]
// defaults its target to the current directory.
func seedSourceCheckoutCwd(t *testing.T) string {
	t.Helper()
	checkout := t.TempDir()
	crd := filepath.Join(checkout, "config", "crd", "bases", "widgets.yaml")
	if err := os.MkdirAll(filepath.Dir(crd), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n"
	if err := os.WriteFile(crd, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(checkout)
	return checkout
}

func TestInitDefaultPathRefusesSourceCheckoutCwd(t *testing.T) {
	checkout := seedSourceCheckoutCwd(t)

	code, _, stderr := runArgs(t, "init")
	if code == 0 {
		t.Fatalf("init inside a source checkout succeeded; stderr = %q", stderr)
	}
	resolved, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"refusing to initialize",
		"kind: Manifest",
		"defaulted to the current directory",
		"goobers init ./my-instance",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("init stderr = %q, missing %q", stderr, want)
		}
	}
	if !strings.Contains(stderr, checkout) && !strings.Contains(stderr, resolved) {
		t.Fatalf("init stderr = %q, missing resolved target path %q", stderr, resolved)
	}
	for _, name := range []string{"instance.yaml", "gaggles", "scheduler", "telemetry.db"} {
		if _, statErr := os.Stat(filepath.Join(checkout, name)); !os.IsNotExist(statErr) {
			t.Fatalf("refused init wrote %s, stat error = %v", name, statErr)
		}
	}
}
