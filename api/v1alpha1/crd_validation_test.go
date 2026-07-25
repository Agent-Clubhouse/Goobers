package v1alpha1

import (
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestWorkflowCRDRejectsSyncBaseInScratchWorkspace(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/goobers.dev_workflows.yaml")
	if err != nil {
		t.Fatalf("read Workflow CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decode Workflow CRD: %v", err)
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	runSchema := root.Properties["spec"].Properties["tasks"].Items.Schema.Properties["run"]
	if len(runSchema.XValidations) != 2 {
		t.Fatalf("run schema CEL validations = %d, want 2", len(runSchema.XValidations))
	}
	validation := runSchema.XValidations[1]
	const wantRule = "!has(self.syncBase) || !self.syncBase || !has(self.workspace) || self.workspace != 'scratch'"
	if validation.Rule != wantRule {
		t.Fatalf("run schema CEL rule = %q, want %q", validation.Rule, wantRule)
	}
	if validation.Message != "syncBase requires a repo workspace" {
		t.Fatalf("run schema CEL message = %q", validation.Message)
	}
}

func TestWorkflowCRDRequiresExactlyOneRunForm(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/goobers.dev_workflows.yaml")
	if err != nil {
		t.Fatalf("read Workflow CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decode Workflow CRD: %v", err)
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	runSchema := root.Properties["spec"].Properties["tasks"].Items.Schema.Properties["run"]
	validation := runSchema.XValidations[0]
	if validation.Rule != "has(self.command) != has(self.script)" {
		t.Fatalf("run schema CEL rule = %q, want command/script exclusivity", validation.Rule)
	}
	if validation.Message != "exactly one of command or script is required" {
		t.Fatalf("run schema CEL message = %q", validation.Message)
	}
	if runSchema.Properties["command"].MinItems == nil || *runSchema.Properties["command"].MinItems != 1 {
		t.Fatal("run.command must require at least one argv item")
	}
	if runSchema.Properties["script"].MinLength == nil || *runSchema.Properties["script"].MinLength != 1 {
		t.Fatal("run.script must require at least one character")
	}
}

func TestWorkflowCRDValidatesDSLVersionShape(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/goobers.dev_workflows.yaml")
	if err != nil {
		t.Fatalf("read Workflow CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decode Workflow CRD: %v", err)
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	schema, ok := root.Properties["dslVersion"]
	if !ok {
		t.Fatal("Workflow CRD is missing dslVersion")
	}
	const wantPattern = `^[0-9]+\.[0-9]+$`
	if schema.Pattern != wantPattern {
		t.Fatalf("dslVersion pattern = %q, want %q", schema.Pattern, wantPattern)
	}
}
