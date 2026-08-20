package v1alpha1

import (
	"os"
	"slices"
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

func TestManifestCRDValidatesInstance(t *testing.T) {
	spec := loadCRDSpecSchema(t, "../../config/crd/bases/goobers.dev_manifests.yaml")
	if !slices.Equal(spec.Required, []string{"instance"}) {
		t.Fatalf("Manifest spec required fields = %v, want [instance]", spec.Required)
	}

	instance := spec.Properties["instance"]
	if !slices.Equal(instance.Required, []string{"environment", "name"}) {
		t.Fatalf("Manifest instance required fields = %v, want [environment name]", instance.Required)
	}
	environment := instance.Properties["environment"]
	gotEnvironments := make([]string, len(environment.Enum))
	for i, value := range environment.Enum {
		gotEnvironments[i] = string(value.Raw)
	}
	wantEnvironments := []string{`"dev"`, `"staging"`, `"prod"`}
	if !slices.Equal(gotEnvironments, wantEnvironments) {
		t.Fatalf("Manifest environment enum = %v, want %v", gotEnvironments, wantEnvironments)
	}
}

func TestPredicateCRDsRejectExplicitEmptyValues(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		field     string
		predicate func(apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps
	}{
		{
			name:  "gaggle backlog",
			path:  "../../config/crd/bases/goobers.dev_gaggles.yaml",
			field: "labelPredicate",
			predicate: func(root apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
				return root.Properties["spec"].Properties["backlog"].Properties["labelPredicate"]
			},
		},
		{
			name:  "gaggle backlog field",
			path:  "../../config/crd/bases/goobers.dev_gaggles.yaml",
			field: "fieldPredicate",
			predicate: func(root apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
				return root.Properties["spec"].Properties["backlog"].Properties["fieldPredicate"]
			},
		},
		{
			name:  "workflow trigger",
			path:  "../../config/crd/bases/goobers.dev_workflows.yaml",
			field: "labelPredicate",
			predicate: func(root apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
				trigger := root.Properties["spec"].Properties["triggers"].Items.Schema
				return trigger.Properties["labelPredicate"]
			},
		},
		{
			name:  "workflow trigger field",
			path:  "../../config/crd/bases/goobers.dev_workflows.yaml",
			field: "fieldPredicate",
			predicate: func(root apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
				trigger := root.Properties["spec"].Properties["triggers"].Items.Schema
				return trigger.Properties["fieldPredicate"]
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := os.ReadFile(tt.path)
			if err != nil {
				t.Fatalf("read CRD: %v", err)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatalf("decode CRD: %v", err)
			}

			root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			schema := tt.predicate(*root)
			if schema.MinLength == nil || *schema.MinLength != 1 {
				t.Fatalf("%s minLength = %v, want 1", tt.field, schema.MinLength)
			}
		})
	}
}
