package ir

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

// TestReferenceWorkflowsNormalizeDeterministically is the acceptance-criterion
// guard for "every shipped DSL definition normalizes to IR deterministically":
// it walks the REAL reference-workflows/ definitions (this repo's own dogfood
// config) rather than hand-built synthetic workflow.Definition values, asserts
// Normalize succeeds on each one, and asserts Document.Digest() is stable
// across two independent calls on the same normalized input.
func TestReferenceWorkflowsNormalizeDeterministically(t *testing.T) {
	root := filepath.Join("..", "..", "..", "reference-workflows", "gaggles", "goobers", "workflows")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read reference workflows dir: %v", err)
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		files = append(files, entry.Name())
	}
	if len(files) == 0 {
		t.Fatal("no reference workflow YAML files found")
	}

	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			var w apiv1.Workflow
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal %s: %v", file, err)
			}
			def := workflow.Definition{Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec}

			first, err := Normalize(def)
			if err != nil {
				t.Fatalf("Normalize(%s) = %v, want every shipped workflow to normalize", file, err)
			}
			second, err := Normalize(def)
			if err != nil {
				t.Fatalf("second Normalize(%s) = %v", file, err)
			}

			d1, err := first.Digest()
			if err != nil {
				t.Fatalf("Digest() on first normalization of %s: %v", file, err)
			}
			d2, err := second.Digest()
			if err != nil {
				t.Fatalf("Digest() on second normalization of %s: %v", file, err)
			}
			if d1 != d2 {
				t.Fatalf("%s: IR digest not deterministic across independent Normalize calls: %s != %s", file, d1, d2)
			}
		})
	}
}
