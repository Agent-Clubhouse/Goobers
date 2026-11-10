package child

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
)

// Exercise the production schema/cross-definition loader against exactly the
// tree mounted by the overlay, including both scheduled parent workflows.
func TestMountedConfig(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"manifest.yaml":   "config/manifest.yaml",
		"gaggle.yaml":     "config/gaggles/reference/gaggle.yaml",
		"goober.yaml":     "config/gaggles/reference/goobers/child-parent/goober.yaml",
		"instructions.md": "config/gaggles/reference/goobers/child-parent/instructions.md",
		"happy.yaml":      "config/gaggles/reference/workflows/kind-child-happy.yaml",
		"failover.yaml":   "config/gaggles/reference/workflows/kind-child-failover.yaml",
		"instance.yaml":   "instance.yaml",
	}
	for source, target := range files {
		raw, err := os.ReadFile(filepath.Join("config", source))
		if err != nil {
			t.Fatal(err)
		}
		target = filepath.Join(root, target)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil {
		t.Fatalf("mounted definitions: %v; report=%+v", err, report)
	}
	if len(set.Workflows) != 2 {
		t.Fatalf("workflows=%d", len(set.Workflows))
	}
	path := filepath.Join(root, "instance.yaml")
	cfg, err := instance.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EngineChildWorkflowsEnabled() {
		t.Fatal("mounted config disables engine child workflows")
	}
	// Advisory proposal validation uses the same catalog as runtime admission.
	source, err := os.ReadFile("fixture/generated-child.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kind-child-happy", "kind-child-failover"} {
		admission, err := childworkflow.ConfiguredAdmission(childworkflow.ParentSelection{Gaggle: "reference", Workflow: name, Stage: "parent"}, childworkflow.BackendEngine, cfg, set, childworkflow.OfflineGooberAdmission)
		if err != nil {
			t.Fatal(err)
		}
		validator, err := childworkflow.NewValidator(admission)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := validator.Validate(source); err != nil {
			t.Fatalf("%s child proposal: %v", name, err)
		}
	}
}
