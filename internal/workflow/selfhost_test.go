package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestSelfhostWorkflowsCompile is #124's divergence guard: it compiles the
// REAL selfhost/ definitions (this repo's own dogfood config) directly,
// against the compiler's full admission checks (capabilities, harness, and
// gate-outcome coverage). testdata/shipped/*.yaml are separately maintained,
// deliberately minimal synthetic fixtures pinned to golden digests — nothing
// previously compiled the actual selfhost YAML, so it could (and did, per
// #124's architect review of testdata/shipped/implementation.yaml) drift
// invalid without any test catching it.
func TestSelfhostWorkflowsCompile(t *testing.T) {
	root := filepath.Join("..", "..", "selfhost", "gaggles", "goobers")

	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"implementer", "reviewer", "curator", "nominator", "analyst", "config-author"} {
		var g apiv1.Goober
		raw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		if err := yaml.Unmarshal(raw, &g); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		goobers[g.Name] = g.Spec
	}

	for _, file := range []string{"implementation.yaml", "backlog-curation.yaml", "work-nomination.yaml", "tutor.yaml"} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, "workflows", file))
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			var w apiv1.Workflow
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal %s: %v", file, err)
			}
			def := Definition{Name: w.Name, Version: 1, Spec: w.Spec}
			if _, err := Compile(def, WithGoobers(goobers)); err != nil {
				t.Fatalf("compile %s against selfhost's real goobers: %v", file, err)
			}
		})
	}
}
