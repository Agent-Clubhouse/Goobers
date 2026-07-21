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
	if len(runSchema.XValidations) != 1 {
		t.Fatalf("run schema CEL validations = %d, want 1", len(runSchema.XValidations))
	}
	validation := runSchema.XValidations[0]
	const wantRule = "!has(self.syncBase) || !self.syncBase || !has(self.workspace) || self.workspace != 'scratch'"
	if validation.Rule != wantRule {
		t.Fatalf("run schema CEL rule = %q, want %q", validation.Rule, wantRule)
	}
	if validation.Message != "syncBase requires a repo workspace" {
		t.Fatalf("run schema CEL message = %q", validation.Message)
	}
}
