package bootstrap

import (
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

const fixtureRoot = "../../test/fixtures/e2e/walking-skeleton"

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
