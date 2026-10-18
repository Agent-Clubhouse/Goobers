package workflow

import (
	"errors"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func childPolicyDefinition() Definition {
	return Definition{Name: "children", DSLVersion: "3.1", Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "parent",
		Tasks: []apiv1.Task{{Name: "parent", Type: apiv1.TaskAgentic, Goal: "delegate", Goober: "coder",
			ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}, AllowedCapabilities: []string{"repo:read"}},
		}},
	}}
}

func TestChildWorkflowPolicyAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Definition)
		want   string
	}{
		{"valid", func(*Definition) {}, ""},
		{"old pin", func(d *Definition) { d.DSLVersion = "3.0" }, "requires dslVersion"},
		{"older pin", func(d *Definition) { d.DSLVersion = "2.0" }, "requires dslVersion"},
		{"missing pin", func(d *Definition) { d.DSLVersion = "" }, "requires dslVersion"},
		{"deterministic", func(d *Definition) { d.Spec.Tasks[0].Type = apiv1.TaskDeterministic }, "requires type=agentic"},
		{"empty goobers", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedGoobers = nil }, "allowedGoobers must contain"},
		{"unknown goober", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedGoobers = []string{"missing"} }, "unknown Goober"},
		{"foreign gaggle", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedGoobers = []string{"foreign"} }, "another gaggle"},
		{"duplicate goober", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedGoobers = []string{"coder", "coder"} }, "duplicate"},
		{"whitespace goober", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedGoobers = []string{" "} }, "invalid name"},
		{"many goobers", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedGoobers = make([]string, 129) }, "1 to 128"},
		{"unknown capability", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedCapabilities = []string{"repo:teleport"} }, "unknown or runner-only capability"},
		{"runner capability", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedCapabilities = []string{"configrepo:read"} }, "unknown or runner-only capability"},
		{"duplicate capability", func(d *Definition) {
			d.Spec.Tasks[0].ChildWorkflows.AllowedCapabilities = []string{"repo:read", "repo:read"}
		}, "duplicate"},
		{"many capabilities", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedCapabilities = make([]string, 129) }, "at most 128"},
		{"no capability grants", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.AllowedCapabilities = nil }, ""},
		{"maximum", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.MaxChildren = 32 }, ""},
		{"excessive", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.MaxChildren = 33 }, "maxChildren"},
		{"negative", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows.MaxChildren = -1 }, "maxChildren"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := childPolicyDefinition()
			tc.change(&def)
			goobers := map[string]apiv1.GooberSpec{"coder": {Gaggle: "web"}, "foreign": {Gaggle: "elsewhere"}}
			_, err := Compile(def, WithGoobers(goobers), WithPreviewFeatures(true))
			problems := CheckWorkflowAdmission(def, goobers)
			if tc.want == "" {
				if err != nil || len(problems) != 0 {
					t.Fatalf("compile=%v admission=%v", err, problems)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("compile=%v; want %q", err, tc.want)
			}
			if !strings.Contains(strings.Join(problems, ";"), tc.want) {
				t.Fatalf("admission=%v; want %q", problems, tc.want)
			}
		})
	}
}

func TestChildWorkflowPreviewAndCatalogBoundaries(t *testing.T) {
	def := childPolicyDefinition()
	if _, err := Compile(def); err == nil || !strings.Contains(err.Error(), "preview") {
		t.Fatalf("missing preview refused: %v", err)
	}
	if _, err := Compile(def, WithPreviewFeatures(true)); err != nil {
		t.Fatalf("runtime compilation without catalog: %v", err)
	}
	if _, err := Compile(def, WithPreviewFeatures(true), WithGoobers(nil)); err == nil || !strings.Contains(err.Error(), "unknown Goober") {
		t.Fatalf("explicit empty catalog: %v", err)
	}
	def.Spec.Tasks[0].ChildWorkflows.MaxChildren = 4
	def.Spec.Tasks[0].ChildWorkflows.AllowPRPublication = true
	features, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatal(err)
	}
	ids := featureIDStrings(features)
	for _, feature := range childWorkflowFeatures() {
		if !slices.Contains(ids, string(feature.ID)) {
			t.Errorf("missing used feature %s", feature.ID)
		}
		discovered, ok := LookupFeature(feature.ID)
		if !ok || discovered.Level != SupportPreview || discovered.SinceVersion != "dev" {
			t.Fatalf("registry=%+v %v", discovered, ok)
		}
		old, err := FeaturesAtDSLVersion([]Feature{feature}, "3.0")
		if err != nil || len(old) != 0 {
			t.Fatalf("old version exposes child policy: %v %v", old, err)
		}
	}
	if !errors.Is(RefuseChildWorkflowExecution(def.Spec), ErrChildWorkflowExecutionUnsupported) {
		t.Fatal("missing execution refusal")
	}
	def.Spec.Tasks[0].ChildWorkflows = nil
	if err := RefuseChildWorkflowExecution(def.Spec); err != nil {
		t.Fatalf("omission changed old workflows: %v", err)
	}
}
