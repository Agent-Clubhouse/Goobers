package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
)

const manifestParityDoc = `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: %s
spec:
  instance:
    name: acme
    environment: dev
`

// twoManifestDir builds a minimal, otherwise schema-valid config directory
// containing two distinct Manifest documents (different metadata.name, so
// only the "more than one Manifest" check fires, not the unrelated
// duplicate-name check) and nothing else — no gaggles declared, so there are
// no dangling cross-references to muddy the result.
func twoManifestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest-a.yaml"), []byte(fmt.Sprintf(manifestParityDoc, "manifest-a")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest-b.yaml"), []byte(fmt.Sprintf(manifestParityDoc, "manifest-b")), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestManifestExactlyOneParityBetweenValidateAndAssemble is #243's acceptance
// test for the manifest "exactly one" gap: before the fix, api/validate only
// Warned on more than one Manifest while LoadConfigDir's own assemble() step
// hard-rejected the identical directory — a validate-only consumer (e.g. the
// `validate` CLI, or the operator's admission path) would report
// success-with-warning for a config LoadConfigDir actually refuses to load.
// Both entry points must now agree.
func TestManifestExactlyOneParityBetweenValidateAndAssemble(t *testing.T) {
	dir := twoManifestDir(t)

	v, err := validate.New()
	if err != nil {
		t.Fatalf("validate.New: %v", err)
	}
	report, err := v.ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !report.HasErrors() {
		t.Fatalf("ValidateDir report has no errors, want an error for more than one Manifest: %+v", report.Issues)
	}
	found := false
	for _, iss := range report.Issues {
		if iss.Severity == validate.Error && strings.Contains(iss.Message, "more than one Manifest") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ValidateDir report missing an error-severity \"more than one Manifest\" issue: %+v", report.Issues)
	}

	set, loadReport, loadErr := LoadConfigDir(dir)
	if !errors.Is(loadErr, ErrInvalidConfig) {
		t.Fatalf("LoadConfigDir err = %v, want ErrInvalidConfig — same verdict as ValidateDir", loadErr)
	}
	if set != nil {
		t.Fatalf("LoadConfigDir returned a non-nil ConfigSet for an invalid (multi-manifest) directory: %+v", set)
	}
	if loadReport == nil || !loadReport.HasErrors() {
		t.Fatalf("LoadConfigDir report has no errors, want the same multi-manifest error ValidateDir reported: %+v", loadReport)
	}
}
