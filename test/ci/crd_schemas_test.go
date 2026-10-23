package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestVendoredCRDSchemasMatchRecordedChecksums keeps the CRD schemas that
// deploy-validate uses byte-identical to the pinned upstream copies recorded in
// test/fixtures/crd-schemas/README.md, and every vendored file recorded.
func TestVendoredCRDSchemasMatchRecordedChecksums(t *testing.T) {
	t.Parallel()
	root := filepath.Join(moduleRoot(t), "test", "fixtures", "crd-schemas")
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := make(map[string]string)
	for _, match := range regexp.MustCompile("(?m)^\\| `([^`]+\\.json)` \\| `([0-9a-f]{64})` \\|$").FindAllStringSubmatch(string(readme), -1) {
		recorded[match[1]] = match[2]
	}
	files, err := filepath.Glob(filepath.Join(root, "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || len(files) != len(recorded) {
		t.Fatalf("%d vendored schemas, %d recorded checksums", len(files), len(recorded))
	}
	for _, file := range files {
		relative, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != recorded[filepath.ToSlash(relative)] {
			t.Errorf("%s has SHA-256 %s, recorded %q", relative, got, recorded[filepath.ToSlash(relative)])
		}
	}
}
