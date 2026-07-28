package authoring

import (
	"errors"
	"reflect"
	"strings"
	"testing"

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
	}{
		{"workflow.spec.gates[].evaluator", "string", []any{"automated", "agentic", "human"}, &required, "automated", "ga"},
		{"goober/spec/mcpServers[]/credentialRefs[]/scheme", "string", []any{"bearer", "basic"}, &optional, "bearer", "ga"},
		{"goober.spec.capabilities", "array", nil, &optional, []any{"repo:read"}, "ga"},
		{"gaggle.spec.sandbox", "object", nil, &optional, map[string]any{}, "preview"},
		{"goober.apiVersion", "string", []any{"goobers.dev/v1alpha1"}, &required, "goobers.dev/v1alpha1", "ga"},
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
				strings.TrimSpace(got.Description) == "" ||
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
	if list.Description != element.Description {
		t.Fatalf("element description = %q, want array purpose %q", element.Description, list.Description)
	}

	workspace, err := Explain("workflow.spec.tasks[].run.workspace")
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{"repo", "scratch"}; !reflect.DeepEqual(workspace.AllowedValues, want) {
		t.Fatalf("workspace values = %#v, want %#v", workspace.AllowedValues, want)
	}
}

func TestExplainRejectsMissingPurposeMetadata(t *testing.T) {
	for _, selector := range []string{
		"features.version",
		"remediation-brief-v2.gatherPrContext.verdict",
		"remediation-brief-v2.gatherPrContext.verdict.decision",
		"remediation-brief-v2.gatherPrContext.comments[]",
	} {
		_, err := Explain(selector)
		if !errors.Is(err, ErrIncompleteContract) {
			t.Errorf("%q: error = %v, want ErrIncompleteContract", selector, err)
		}
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
