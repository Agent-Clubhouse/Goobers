package workflow

import (
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// Exercise the actual shipped version pins and handoff declarations. Keep the
// existing 3.1 selective-parallel example on its separately opted-in version.
func TestReferenceWorkflowsUseDSL3(t *testing.T) {
	files, err := filepath.Glob("../../reference-workflows/gaggles/goobers/workflows/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 17 {
		t.Fatalf("reference workflow count = %d, want 17", len(files))
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var wf apiv1.Workflow
			if err := yaml.Unmarshal(source, &wf); err != nil {
				t.Fatal(err)
			}
			want := "3.0"
			if wf.Name == "quality-spot-check" {
				want = "3.1"
			}
			if wf.DSLVersion != want {
				t.Fatalf("dslVersion = %s, want %s", wf.DSLVersion, want)
			}
			def := Definition{Name: wf.Name, DSLVersion: wf.DSLVersion, Spec: wf.Spec, Annotations: wf.Annotations}
			if _, err := Compile(def, WithPreviewFeatures(PreviewFeaturesEnabled(wf.Annotations))); err != nil {
				t.Fatalf("compile shipped pin: %v", err)
			}
			if findings := CheckRepoHandoffs(def); len(findings) != 0 {
				t.Fatalf("repository handoffs: %v", findings)
			}
		})
	}
}
