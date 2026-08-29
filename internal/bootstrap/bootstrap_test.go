package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

const fixtureRoot = "../../test/fixtures/e2e/walking-skeleton"

// TestRegisterGaggleWorkflowsPreservesDSLVersion is the regression test for
// the bug found live against aks-goobernetes-prod 2026-08-24: Register (the
// two-arg shim) drops DSLVersion, so every workflow registered through
// RegisterGaggleWorkflows silently defaulted to supportmatrix.CurrentDSLVersion
// (1.4) regardless of what it actually declared, and a 3.0 workflow's runsOn
// then refused as if authored against 1.4. Copies the walking-skeleton
// fixture (whose manifest already opts into preview features) and adds a
// second, 3.0, runsOn-bearing workflow alongside the existing 2.0 one.
func TestRegisterGaggleWorkflowsPreservesDSLVersion(t *testing.T) {
	root := copyFixtureWithExtraWorkflow(t, fixtureRoot, "gaggles/acme-web/workflows/runson-3-0.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.0"
metadata:
  name: runson-3-0
spec:
  gaggle: acme-web
  triggers:
    - type: manual
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Implement.
      capabilities: [agent:model]
      runsOn:
        os: linux
`)
	set, report, err := instance.LoadConfigDir(root)
	if err != nil {
		t.Fatalf("load config dir: %v", err)
	}
	if report != nil && report.HasErrors() {
		t.Fatalf("config report has errors: %v", report)
	}

	reg, _, err := RegisterGaggleWorkflows(set, "acme-web")
	if err != nil {
		t.Fatalf("RegisterGaggleWorkflows: %v (a 3.0 workflow with runsOn must not be coerced to an earlier DSL version)", err)
	}
	def, ok := reg.Latest("runson-3-0")
	if !ok {
		t.Fatal("runson-3-0 was not registered")
	}
	if def.DSLVersion != "3.0" {
		t.Fatalf("DSLVersion = %q, want \"3.0\" (dropped/defaulted instead of carried through)", def.DSLVersion)
	}
}

// copyFixtureWithExtraWorkflow copies srcRoot into a fresh temp dir and
// writes an additional file at relPath, returning the temp dir's path.
func copyFixtureWithExtraWorkflow(t *testing.T, srcRoot, relPath, content string) string {
	t.Helper()
	dst := t.TempDir()
	if err := filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	extra := filepath.Join(dst, relPath)
	if err := os.MkdirAll(filepath.Dir(extra), 0o755); err != nil {
		t.Fatalf("mkdir for extra workflow: %v", err)
	}
	if err := os.WriteFile(extra, []byte(content), 0o644); err != nil {
		t.Fatalf("write extra workflow: %v", err)
	}
	return dst
}

func TestRegisterGaggleWorkflowsFixture(t *testing.T) {
	set, report, err := instance.LoadConfigDir(fixtureRoot)
	if err != nil {
		t.Fatalf("load config dir: %v", err)
	}
	if report != nil && report.HasErrors() {
		t.Fatalf("config report has errors: %v", report)
	}
	if len(set.Gaggles) == 0 || len(set.Workflows) == 0 {
		t.Fatalf("expected gaggles + workflows in fixture, got %d gaggles, %d workflows", len(set.Gaggles), len(set.Workflows))
	}
	gaggle := set.Gaggles[0]

	reg, project, err := RegisterGaggleWorkflows(set, gaggle.Name)
	if err != nil {
		t.Fatalf("RegisterGaggleWorkflows: %v", err)
	}
	if project != gaggle.Spec.Project {
		t.Errorf("project = %+v, want %+v", project, gaggle.Spec.Project)
	}
	// Every workflow belonging to the gaggle is registered and resolvable;
	// every workflow belonging to a different gaggle is not.
	for _, w := range set.Workflows {
		def, ok := reg.Latest(w.Name)
		if w.Spec.Gaggle != gaggle.Name {
			if ok {
				t.Errorf("workflow %q belongs to gaggle %q, but was registered", w.Name, w.Spec.Gaggle)
			}
			continue
		}
		if !ok {
			t.Errorf("workflow %q was not registered", w.Name)
			continue
		}
		if _, err := reg.Compile(def); err != nil {
			t.Errorf("registered workflow %q does not compile: %v", w.Name, err)
		}
	}
}

func TestRegisterGaggleWorkflowsUnknownGaggleRegistersNothing(t *testing.T) {
	set, report, err := instance.LoadConfigDir(fixtureRoot)
	if err != nil {
		t.Fatalf("load config dir: %v", err)
	}
	if report != nil && report.HasErrors() {
		t.Fatalf("config report has errors: %v", report)
	}

	reg, project, err := RegisterGaggleWorkflows(set, "does-not-exist")
	if err != nil {
		t.Fatalf("RegisterGaggleWorkflows: %v", err)
	}
	if project.Owner != "" || project.Name != "" {
		t.Errorf("project = %+v, want zero value for an unknown gaggle", project)
	}
	for _, w := range set.Workflows {
		if _, ok := reg.Latest(w.Name); ok {
			t.Errorf("workflow %q was registered under an unknown gaggle", w.Name)
		}
	}
}
