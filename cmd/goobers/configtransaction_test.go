package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestUpRecoversConfigBeforeRootValidation(t *testing.T) {
	root := t.TempDir()
	// Model a persisted pending transaction with both live paths absent. The old
	// instance intentionally fails semantic preflight, so this never starts a daemon.
	dir := filepath.Join(root, ".config-transaction")
	if err := os.MkdirAll(filepath.Join(dir, "old", instance.ConfigDirName), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old", instance.ConfigFileName), []byte("invalid: [yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "intent"), []byte(`{"schema":1,"withInstance":true,"committed":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runArgs(t, "up", root)
	if code == 0 || !strings.Contains(stderr, "Instance root:") {
		t.Fatalf("startup did not recover before root validation: %d %s", code, stderr)
	}
	got, err := os.ReadFile(filepath.Join(root, instance.ConfigFileName))
	if err != nil || string(got) != "invalid: [yaml" {
		t.Fatalf("restored instance = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, instance.ConfigDirName)); err != nil {
		t.Fatal(err)
	}
}
