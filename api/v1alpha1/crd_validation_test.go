package v1alpha1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
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
	if len(runSchema.XValidations) != 3 {
		t.Fatalf("run schema CEL validations = %d, want 3", len(runSchema.XValidations))
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

func TestWorkflowCRDRejectsEmptyExecutableName(t *testing.T) {
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
	validation := runSchema.XValidations[2]
	const wantRule = "!has(self.command) || size(self.command[0]) > 0"
	if validation.Rule != wantRule {
		t.Fatalf("run schema CEL rule = %q, want %q", validation.Rule, wantRule)
	}
	if validation.Message != "command[0] must name an executable" {
		t.Fatalf("run schema CEL message = %q", validation.Message)
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

// TestCRDsAreInstallable runs the API server's own CustomResourceDefinition
// validator over every shipped CRD. An uninstallable schema — a forbidden
// uniqueItems marker, or a CEL rule whose estimated cost exceeds the per-rule
// budget because an enclosing array is unbounded — used to surface only in the
// operator's envtest tier, which needs a downloaded control plane and so does
// not run in the unit tier (#3166, fixed in #3168). This catches the same
// rejection in milliseconds, with no control plane.
func TestCRDsAreInstallable(t *testing.T) {
	paths, err := filepath.Glob("../../config/crd/bases/*.yaml")
	if err != nil {
		t.Fatalf("glob CRDs: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no CRDs found under config/crd/bases")
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if errs := validateCRDForInstall(t, path, nil); len(errs) != 0 {
				t.Fatalf("CRD is not installable: %v", errs)
			}
		})
	}
}

// TestCRDInstallCheckDetectsForbiddenSchema is the negative control for
// TestCRDsAreInstallable: it reintroduces the two constraints that made the
// Workflow CRD uninstallable and asserts the check rejects them, so the guard
// above cannot silently degrade into a test that passes on anything.
func TestCRDInstallCheckDetectsForbiddenSchema(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*apiextensionsv1.JSONSchemaProps)
		want   string
	}{
		{
			name: "uniqueItems",
			mutate: func(root *apiextensionsv1.JSONSchemaProps) {
				task := taskSchema(root)
				contextFrom := task.Properties["contextFrom"]
				contextFrom.UniqueItems = true
				task.Properties["contextFrom"] = contextFrom
			},
			want: "uniqueItems",
		},
		{
			name: "unbounded array around a CEL rule",
			mutate: func(root *apiextensionsv1.JSONSchemaProps) {
				spec := root.Properties["spec"]
				tasks := spec.Properties["tasks"]
				tasks.MaxItems = nil
				spec.Properties["tasks"] = tasks
				root.Properties["spec"] = spec
			},
			want: "estimated rule cost exceeds budget",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateCRDForInstall(t, "../../config/crd/bases/goobers.dev_workflows.yaml", tt.mutate)
			joined := fmt.Sprint(errs)
			if !strings.Contains(joined, tt.want) {
				t.Fatalf("errors = %v, want one containing %q", errs, tt.want)
			}
		})
	}
}

// taskSchema returns the mutable per-task schema, copying the shared item
// schema so a mutation cannot leak across subtests.
func taskSchema(root *apiextensionsv1.JSONSchemaProps) *apiextensionsv1.JSONSchemaProps {
	spec := root.Properties["spec"]
	tasks := spec.Properties["tasks"]
	item := *tasks.Items.Schema
	tasks.Items.Schema = &item
	spec.Properties["tasks"] = tasks
	root.Properties["spec"] = spec
	return &item
}

// validateCRDForInstall decodes a shipped CRD, applies the status the API
// server derives on create, and validates it exactly as the API server does on
// `kubectl apply`. mutate may perturb the decoded schema first.
func validateCRDForInstall(t *testing.T, path string, mutate func(*apiextensionsv1.JSONSchemaProps)) field.ErrorList {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decode CRD: %v", err)
	}
	if mutate != nil {
		mutate(crd.Spec.Versions[0].Schema.OpenAPIV3Schema)
	}

	var internal apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, &internal, nil); err != nil {
		t.Fatalf("convert CRD: %v", err)
	}
	internal.Status.AcceptedNames = internal.Spec.Names
	for _, version := range internal.Spec.Versions {
		if version.Storage {
			internal.Status.StoredVersions = append(internal.Status.StoredVersions, version.Name)
		}
	}

	return validation.ValidateCustomResourceDefinition(context.Background(), &internal)
}

// TestWorkflowCRDPinsGateCELRules pins the exact text of the two gate-level
// CEL rules: the maxRepasses evaluator rule and decision 001 ruling 2's
// "runsOn is only valid for agentic gates". `make manifests-check`
// regenerates from the Go markers and diffs, so deleting a marker regenerates
// a CRD without the rule and the diff is still empty — only a test that reads
// the committed rule can notice its removal (the precedent
// TestWorkflowCRDRejectsSyncBaseInScratchWorkspace sets for run).
func TestWorkflowCRDPinsGateCELRules(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/goobers.dev_workflows.yaml")
	if err != nil {
		t.Fatalf("read Workflow CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decode Workflow CRD: %v", err)
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	gateSchema := root.Properties["spec"].Properties["gates"].Items.Schema
	if len(gateSchema.XValidations) != 2 {
		t.Fatalf("gate schema CEL validations = %d, want 2 (maxRepasses, runsOn)", len(gateSchema.XValidations))
	}
	maxRepasses := gateSchema.XValidations[0]
	if maxRepasses.Rule != "!has(self.maxRepasses) || self.evaluator != 'human'" {
		t.Fatalf("gate maxRepasses CEL rule = %q", maxRepasses.Rule)
	}
	if maxRepasses.Message != "maxRepasses is only valid for automated or agentic gates" {
		t.Fatalf("gate maxRepasses CEL message = %q", maxRepasses.Message)
	}
	runsOn := gateSchema.XValidations[1]
	if runsOn.Rule != "!has(self.runsOn) || self.evaluator == 'agentic'" {
		t.Fatalf("gate runsOn CEL rule = %q, want decision 001 ruling 2 (only agentic gates are placeable)", runsOn.Rule)
	}
	if runsOn.Message != "runsOn is only valid for agentic gates" {
		t.Fatalf("gate runsOn CEL message = %q", runsOn.Message)
	}
	if _, ok := gateSchema.Properties["runsOn"]; !ok {
		t.Fatal("gate schema must carry the runsOn property the rule guards")
	}
}
