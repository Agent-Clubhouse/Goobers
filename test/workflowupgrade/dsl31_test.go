package workflowupgrade

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/instance"
)

func TestDSL31MechanicalUpgradeLoadsWithoutExpectedOutputs(t *testing.T) {
	before, err := os.ReadFile("../../internal/dslmigrate/testdata/v3_0/undeclared-outputs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	result, err := dslmigrate.Migrate(before, "3.1")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(path, []byte(result.After), 0o644); err != nil {
		t.Fatal(err)
	}
	set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil {
		t.Fatalf("target loader rejected migrated workflow: %v\n%+v", err, report)
	}
	if len(set.Workflows) != 1 || set.Workflows[0].DSLVersion != "3.1" {
		t.Fatalf("loaded workflows = %+v, want one at DSL 3.1", set.Workflows)
	}
	for _, task := range set.Workflows[0].Spec.Tasks {
		if task.ExpectedOutputs != nil || task.ArtifactSlots != nil || task.ArtifactInputs != nil {
			t.Fatalf("migration or loading invented output declarations: %+v", task)
		}
	}
}
