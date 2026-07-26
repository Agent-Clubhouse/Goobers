package dslmigrate_test

import (
	"os"
	"testing"

	k8syaml "sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/workflow"
)

// TestMigratedFixtureCompilesAtTargetVersion proves the acceptance criterion
// from #866: `fix --to <next>` on a v_current workflow produces a diff that
// makes it a valid v_next workflow. It migrates the real DVL-4/DVL-5 golden
// fixture (the same one internal/workflow/v_current and .../v_next test
// against) and compiles the result with the shared version-router Compile,
// the same entry point `goobers up`/`run` use.
func TestMigratedFixtureCompilesAtTargetVersion(t *testing.T) {
	for _, fixture := range []string{"gated", "linear", "runtime-policy"} {
		t.Run(fixture, func(t *testing.T) {
			source, err := os.ReadFile("../workflow/v_current/testdata/golden/" + fixture + ".yaml")
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			result, err := dslmigrate.Migrate(source, "2.0")
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}

			var wf apiv1.Workflow
			if err := k8syaml.Unmarshal([]byte(result.After), &wf); err != nil {
				t.Fatalf("decode migrated workflow: %v\n%s", err, result.After)
			}
			if wf.DSLVersion != "2.0" {
				t.Fatalf("migrated dslVersion = %q, want 2.0", wf.DSLVersion)
			}

			if _, err := workflow.Compile(workflow.Definition{
				Name:       wf.Name,
				DSLVersion: wf.DSLVersion,
				Spec:       wf.Spec,
			}); err != nil {
				t.Fatalf("migrated workflow does not compile at dslVersion 2.0: %v\n%s", err, result.After)
			}
		})
	}
}
