package validate

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// TestBackpropEnrollmentRequirementMatchesWorkflowCRD pins that the canonical
// workflow JSON schema and the Workflow CRD's admission rules agree on which
// backprop objects carry an enrollment choice (enabled or mode) and on the
// rejection of enabled=true alongside a non-active mode.
func TestBackpropEnrollmentRequirementMatchesWorkflowCRD(t *testing.T) {
	v := newV(t)
	validator := workflowCRDBackpropValidator(t)
	for _, test := range []struct {
		name     string
		backprop map[string]any
		accepted bool
	}{
		{name: "version only", backprop: map[string]any{"version": "v1"}},
		{name: "enabled", backprop: map[string]any{"enabled": true, "version": "v1"}, accepted: true},
		{name: "enabled false", backprop: map[string]any{"enabled": false, "version": "v1"}, accepted: true},
		{name: "mode", backprop: map[string]any{"mode": "shadow", "version": "v1"}, accepted: true},
		{name: "enabled and mode", backprop: map[string]any{"enabled": true, "mode": "active", "version": "v1"}, accepted: true},
		{name: "enabled with shadow", backprop: map[string]any{"enabled": true, "mode": "shadow", "version": "v1"}},
		{name: "enabled with off", backprop: map[string]any{"enabled": true, "mode": "off", "version": "v1"}},
		{name: "disabled with shadow", backprop: map[string]any{"enabled": false, "mode": "shadow", "version": "v1"}, accepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := json.Marshal(map[string]any{
				"apiVersion": "goobers.dev/v1alpha1", "kind": "Workflow",
				"metadata": map[string]any{"name": "backprop-flow"}, "dslVersion": "3.0",
				"spec": map[string]any{
					"gaggle": "example", "backprop": test.backprop,
					"triggers": []any{map[string]any{"type": "manual"}}, "start": "act",
					"tasks": []any{map[string]any{
						"name": "act", "type": "deterministic", "goal": "Act.",
						"run": map[string]any{"command": []any{"true"}},
					}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			schemaErr := v.ValidateJSON("workflow.schema.json", doc)
			if (schemaErr == nil) != test.accepted {
				t.Fatalf("workflow.schema.json accepted = %v (%v), want %v", schemaErr == nil, schemaErr, test.accepted)
			}
			crdErrs := validator(test.backprop)
			if (len(crdErrs) == 0) != test.accepted {
				t.Fatalf("Workflow CRD accepted = %v (%v), want %v", len(crdErrs) == 0, crdErrs, test.accepted)
			}
		})
	}
}

// API server CEL limits (k8s.io/apiserver/pkg/apis/cel), inlined to keep that
// module an indirect dependency.
const (
	celPerCallLimit      = 1000000
	celRuntimeCostBudget = 10000000
)

// workflowCRDBackpropValidator evaluates the committed Workflow CRD's CEL
// rules for spec.backprop exactly as the API server does on admission.
func workflowCRDBackpropValidator(t *testing.T) func(map[string]any) field.ErrorList {
	t.Helper()
	data, err := os.ReadFile("../../config/crd/bases/goobers.dev_workflows.yaml")
	if err != nil {
		t.Fatalf("read Workflow CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decode Workflow CRD: %v", err)
	}
	props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["backprop"]
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&props, &internal, nil); err != nil {
		t.Fatalf("convert backprop schema: %v", err)
	}
	structural, err := schema.NewStructural(&internal)
	if err != nil {
		t.Fatalf("structural backprop schema: %v", err)
	}
	validator := cel.NewValidator(structural, false, celPerCallLimit)
	if validator == nil {
		t.Fatal("Workflow CRD spec.backprop carries no CEL validation rules")
	}
	return func(obj map[string]any) field.ErrorList {
		errs, _ := validator.Validate(context.Background(), field.NewPath("spec", "backprop"), structural, obj, nil, celRuntimeCostBudget)
		return errs
	}
}
