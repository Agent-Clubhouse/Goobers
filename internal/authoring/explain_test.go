package authoring

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestExplainProjectsSchemaAndRegistryGuidance(t *testing.T) {
	required, optional := true, false
	tests := []struct {
		selector      string
		wantType      any
		wantValues    []any
		wantRequired  *bool
		wantExample   any
		wantStability string
		documented    bool
	}{
		{"workflow.spec.gates[].evaluator", "string", []any{"automated", "agentic", "human"}, &required, "automated", "ga", true},
		{"goober/spec/mcpServers[]/credentialRefs[]/scheme", "string", []any{"bearer", "basic"}, &optional, "bearer", "ga", true},
		{"goober.spec.capabilities", "array", nil, &optional, []any{"repo:read"}, "ga", true},
		{"features.version", "string", nil, &required, "x", "ga", false},
		{"gaggle.spec.sandbox", "object", nil, &optional, map[string]any{}, "preview", true},
		{"goober.apiVersion", "string", []any{"goobers.dev/v1alpha1"}, &required, "goobers.dev/v1alpha1", "ga", true},
		{"remediation-brief-v2.gatherPrContext.verdict", []any{"object", "null"}, nil, &required, map[string]any{"decision": "pass"}, "ga", false},
	}
	for _, test := range tests {
		t.Run(test.selector, func(t *testing.T) {
			got, err := Explain(test.selector)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Type, test.wantType) ||
				!reflect.DeepEqual(got.Required, test.wantRequired) ||
				!reflect.DeepEqual(got.Example, test.wantExample) ||
				got.Stability != test.wantStability ||
				got.Documented != test.documented ||
				got.SinceVersion == "" {
				t.Fatalf("explanation = %+v", got)
			}
			if test.wantValues != nil && !reflect.DeepEqual(got.AllowedValues, test.wantValues) {
				t.Fatalf("allowed values = %#v, want %#v", got.AllowedValues, test.wantValues)
			}
		})
	}

	list, err := Explain("goober.spec.capabilities")
	if err != nil {
		t.Fatal(err)
	}
	element, err := Explain("goober.spec.capabilities[]")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list.AllowedValues, element.AllowedValues) || len(list.AllowedValues) == 0 {
		t.Fatalf("list values = %#v; element values = %#v", list, element)
	}

	workspace, err := Explain("workflow.spec.tasks[].run.workspace")
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{"repo", "scratch"}; !reflect.DeepEqual(workspace.AllowedValues, want) {
		t.Fatalf("workspace values = %#v, want %#v", workspace.AllowedValues, want)
	}
}

func TestExplainRejectsInvalidSelectors(t *testing.T) {
	for _, selector := range []string{"", " goober.spec.role", "goober.", "goober[]", "goober.spec[]", "missing.spec", "goober.unknown", "goober.spec.role[]", "workflow.spec.tasks.name", "workflow/spec.tasks"} {
		_, err := Explain(selector)
		if !errors.Is(err, ErrUnknownSelector) {
			t.Errorf("%q: error = %v, want ErrUnknownSelector", selector, err)
		}
	}
	for _, selector := range []string{"workflow.spec.tasks[].run.script", "Workflow.spec.tasks[].run.script", "workflow.spec.tasks[].workspace", "workflow.spec.gates[].agentic.workspace", "workflow.spec.parallels"} {
		_, err := Explain(selector)
		if !errors.Is(err, ErrUnavailableSelector) ||
			!strings.Contains(err.Error(), supportmatrix.CurrentDSLVersion) {
			t.Errorf("%q: error = %v, want unavailable in DSL %s", selector, err, supportmatrix.CurrentDSLVersion)
		}
	}
}

func TestExplainExamplesSatisfySelectedSchemas(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	for _, file := range schemas.Files() {
		raw, err := schemas.FS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(schemas.BaseURI+file, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	tests := map[string]string{
		"workflow.spec.gates[].branches":               "workflow.schema.json#/$defs/gate/properties/branches",
		"artifact-pointer.digest":                      "artifact-pointer.schema.json#/properties/digest",
		"remediation-brief-v2.gatherPrContext.verdict": "remediation-brief-v2.schema.json#/$defs/gatherPrContext/properties/verdict",
		"goober.spec.mcpServers[]":                     "goober.schema.json#/$defs/mcpServer",
		"invocation.contextPointers[]":                 "invocation.schema.json#/$defs/contextPointer",
	}
	for selector, schema := range tests {
		t.Run(selector, func(t *testing.T) {
			got, err := Explain(selector)
			if err != nil {
				t.Fatal(err)
			}
			compiled, err := compiler.Compile(schemas.BaseURI + schema)
			if err != nil {
				t.Fatal(err)
			}
			if err := compiled.Validate(got.Example); err != nil {
				encoded, _ := json.Marshal(got.Example)
				t.Fatalf("example %s does not satisfy selected schema: %v", encoded, err)
			}
		})
	}
}
