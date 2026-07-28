package authoring

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/workflow"
)

func TestExplainProjectsSchemaAndRegistryGuidance(t *testing.T) {
	tests := []struct {
		selector        string
		wantType        any
		wantValues      []any
		wantRequired    *bool
		wantDescription string
		wantExample     any
	}{
		{
			selector:        "workflow.spec.gates[].evaluator",
			wantType:        "string",
			wantValues:      []any{"automated", "agentic", "human"},
			wantRequired:    boolPointer(true),
			wantDescription: "Evaluator kind configured by exactly one of automated, agentic, or human.",
			wantExample:     "automated",
		},
		{
			selector:        "goober/spec/mcpServers[]/credentialRefs[]/scheme",
			wantType:        "string",
			wantValues:      []any{"bearer", "basic"},
			wantRequired:    boolPointer(false),
			wantDescription: "Optional authorization scheme prefix for a remote header credential.",
			wantExample:     "bearer",
		},
		{
			selector:        "workflow.spec.triggers[].labelPredicate",
			wantType:        "string",
			wantRequired:    boolPointer(false),
			wantDescription: "CEL label-set predicate using only string membership in `labels` and boolean &&, ||, and !. ANDed with selector.",
			wantExample:     "x",
		},
	}

	for _, test := range tests {
		t.Run(test.selector, func(t *testing.T) {
			got, err := Explain(test.selector)
			if err != nil {
				t.Fatal(err)
			}
			if got.Type != test.wantType {
				t.Errorf("type = %#v, want %#v", got.Type, test.wantType)
			}
			if !reflect.DeepEqual(got.AllowedValues, test.wantValues) {
				t.Errorf("allowed values = %#v, want %#v", got.AllowedValues, test.wantValues)
			}
			if !reflect.DeepEqual(got.Required, test.wantRequired) {
				t.Errorf("required = %#v, want %#v", got.Required, test.wantRequired)
			}
			if got.Description != test.wantDescription || !got.Documented {
				t.Errorf("documentation = %q/%t", got.Description, got.Documented)
			}
			if got.Default != nil {
				t.Errorf("schema-absent default was synthesized: %+v", got)
			}
			if got.Stability != schemas.StabilityGA ||
				got.SinceVersion != schemas.InitialSinceVersion ||
				!reflect.DeepEqual(got.Example, test.wantExample) {
				t.Errorf("authoring guidance = %+v", got)
			}
		})
	}
}

func TestExplainMarksUndocumentedFields(t *testing.T) {
	got, err := Explain("features.version")
	if err != nil {
		t.Fatal(err)
	}
	if got.Documented || got.Description != "" {
		t.Fatalf("undocumented field = %+v", got)
	}
	if got.Type != "string" {
		t.Fatalf("type = %#v, want string", got.Type)
	}
	if got.Stability != schemas.StabilityGA ||
		got.SinceVersion != schemas.InitialSinceVersion ||
		got.Example != "x" {
		t.Fatalf("registry guidance = %+v", got)
	}
}

func TestExplainProjectsArrayItemAllowedValues(t *testing.T) {
	list, err := Explain("goober.spec.capabilities")
	if err != nil {
		t.Fatal(err)
	}
	element, err := Explain("goober.spec.capabilities[]")
	if err != nil {
		t.Fatal(err)
	}
	if list.Type != "array" || element.Type != "string" ||
		len(list.AllowedValues) == 0 ||
		!reflect.DeepEqual(list.AllowedValues, element.AllowedValues) {
		t.Fatalf("list values = %#v; element values = %#v", list, element)
	}
}

func TestExplainProjectsAnnotationsWithoutGeneratingValues(t *testing.T) {
	required := true
	defaultValue := any(json.Number("3"))
	declared := map[string]any{
		"description":  "Exact schema prose.",
		"type":         []any{"integer", "null"},
		"enum":         []any{json.Number("3"), nil},
		"default":      defaultValue,
		"sinceVersion": "v1.2.0",
	}
	got := projectFacts("test.value", declared, declared, &required)
	if !got.Documented || got.Description != "Exact schema prose." ||
		!reflect.DeepEqual(got.Type, []any{"integer", "null"}) ||
		!reflect.DeepEqual(got.AllowedValues, []any{json.Number("3"), nil}) ||
		got.Default == nil || !reflect.DeepEqual(*got.Default, defaultValue) ||
		got.SinceVersion != "v1.2.0" {
		t.Fatalf("projection = %+v", got)
	}
}

func TestExplainUsesFeatureLifecycleForVersionGatedFields(t *testing.T) {
	got, err := Explain("gaggle.spec.sandbox")
	if err != nil {
		t.Fatal(err)
	}
	feature, ok := workflow.LookupFeature("gaggle.spec.sandbox")
	if !ok {
		t.Fatal("gaggle.spec.sandbox feature is not registered")
	}
	if got.Stability != string(feature.Level) || got.SinceVersion != feature.SinceVersion {
		t.Fatalf("lifecycle = %s/%s, want %s/%s", got.Stability, got.SinceVersion, feature.Level, feature.SinceVersion)
	}
	if got.Example == nil {
		t.Fatal("preview field has no example")
	}
}

func TestExplainLifecycleIgnoresNextDSLPrefixFeatures(t *testing.T) {
	got, err := Explain("workflow.spec.gates[].evaluator")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stability != string(workflow.SupportGA) {
		t.Fatalf("current-DSL evaluator stability = %q, want %q", got.Stability, workflow.SupportGA)
	}
}

func TestExplainDerivesTypeAndExampleFromConst(t *testing.T) {
	got, err := Explain("goober.apiVersion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "string" ||
		!reflect.DeepEqual(got.AllowedValues, []any{"goobers.dev/v1alpha1"}) ||
		got.Example != "goobers.dev/v1alpha1" {
		t.Fatalf("const guidance = %+v", got)
	}
}

func TestExplainRejectsUnknownSelectors(t *testing.T) {
	for _, selector := range []string{
		"",
		" goober.spec.role",
		"goober.",
		"goober[]",
		"goober.spec[]",
		"missing.spec",
		"goober.unknown",
		"goober.spec.role[]",
		"workflow.spec.tasks.name",
		"workflow/spec.tasks",
	} {
		t.Run(selector, func(t *testing.T) {
			_, err := Explain(selector)
			if !errors.Is(err, ErrUnknownSelector) {
				t.Fatalf("error = %v, want ErrUnknownSelector", err)
			}
			if selector != "" && !strings.Contains(err.Error(), selector) {
				t.Errorf("error %q does not identify selector %q", err, selector)
			}
		})
	}
}

func TestExplainResolvesExternalSchemaReferences(t *testing.T) {
	got, err := Explain("invocation.contextPointers[].artifact.digest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "string" || !strings.Contains(got.Description, "sha256") {
		t.Fatalf("external reference projection = %+v", got)
	}
}

func boolPointer(value bool) *bool {
	return &value
}
