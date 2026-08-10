package authoring

import (
	"errors"
	"reflect"
	"strings"
	"testing"

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
	}{
		{"workflow.spec.gates[].evaluator", "string", []any{"automated", "agentic", "human"}, &required, "automated", "ga"},
		{"goober/spec/mcpServers[]/credentialRefs[]/scheme", "string", []any{"bearer", "basic"}, &optional, "bearer", "ga"},
		{"goober.spec.capabilities", "array", nil, &optional, []any{"repo:read"}, "ga"},
		{"gaggle.spec.sandbox", "object", nil, &optional, map[string]any{}, "preview"},
		{"goober.apiVersion", "string", []any{"goobers.dev/v1alpha1"}, &required, "goobers.dev/v1alpha1", "ga"},
		// instance.yaml selectors: the first file `goobers init` tells an
		// operator to edit had no explain surface at all until #2685's
		// cold-start pass (`goobers explain instance.repos` -> unknown
		// selector), so these pin that it answers like any other kind.
		{"instance.repos", "array", nil, &required, []any{map[string]any{"provider": "github", "owner": "x", "name": "x"}}, "ga"},
		{"instance.repos[].provider", "string", []any{"github", "ado", "gitea"}, &required, "github", "ga"},
		{"instance/repos[]/project", "string", nil, &optional, "x", "ga"},
		{"instance.runner.capabilities", "array", nil, &optional, []any{"dotnet@8"}, "ga"},
		{"instance.runner.defaultStageTimeout", "string", nil, &optional, "25m", "ga"},
		{"instance.repos[].workspace.cleanPolicy", "string", []any{"none", "ignored-safe", "full"}, &optional, "none", "ga"},
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

func TestEveryEmbeddedSelectorReturnsCompleteGuidance(t *testing.T) {
	r := registry{documents: make(map[string]*schemaDocument)}
	selectors := make(map[string]bool)
	for _, entry := range schemas.Entries() {
		doc, err := r.load(entry.Kind)
		if err != nil {
			t.Fatal(err)
		}
		collectSelectors(t, &r, doc, doc.root, entry.Kind, selectors, 0)
	}
	for selector := range selectors {
		got, err := Explain(selector)
		if errors.Is(err, ErrUnavailableSelector) {
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", selector, err)
			continue
		}
		if strings.TrimSpace(got.Description) == "" || got.Type == nil ||
			got.Stability == "" || got.SinceVersion == "" {
			t.Errorf("%q: incomplete explanation: %+v", selector, got)
		}
	}
}

func collectSelectors(t *testing.T, r *registry, doc *schemaDocument, node map[string]any, selector string, selectors map[string]bool, depth int) {
	t.Helper()
	if depth > 32 {
		t.Fatal("schema nesting exceeds selector enumeration depth")
	}
	doc, node, err := r.resolve(doc, node)
	if err != nil {
		t.Fatal(err)
	}
	selectors[selector] = true
	if properties, ok := node["properties"].(map[string]any); ok {
		for name, value := range properties {
			child, ok := value.(map[string]any)
			if !ok {
				continue
			}
			collectSelectors(t, r, doc, child, selector+"."+name, selectors, depth+1)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		collectSelectors(t, r, doc, items, selector+"[]", selectors, depth+1)
	}
	for _, keyword := range []string{"allOf", "oneOf", "anyOf"} {
		alternatives, _ := node[keyword].([]any)
		for _, value := range alternatives {
			alternative, ok := value.(map[string]any)
			if !ok {
				continue
			}
			collectSelectors(t, r, doc, alternative, selector, selectors, depth+1)
		}
	}
}

// The instance.yaml traps the cold-start walkthroughs hit are only fixed if
// the projected guidance names the CONSEQUENCE, not just the field: a runner
// that under-claims never schedules, envPassthrough sits on top of an existing
// allowlist, and the stage-timeout default is sized for short commands.
func TestExplainProjectsInstanceRunnerTrapGuidance(t *testing.T) {
	tests := []struct {
		selector string
		want     []string
	}{
		{"instance.runner.capabilities", []string{"requiredCapabilities", "never schedules a single run"}},
		{"instance.runner.envPassthrough", []string{"default-deny", "Java/Maven/Gradle"}},
		{"instance.runner.defaultStageTimeout", []string{"10 minutes", "timeoutSeconds"}},
		{"instance.repos[].project", []string{"three-part", "Required for provider `ado`"}},
	}
	for _, test := range tests {
		t.Run(test.selector, func(t *testing.T) {
			got, err := Explain(test.selector)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				if !strings.Contains(got.Description, want) {
					t.Errorf("description %q does not mention %q", got.Description, want)
				}
			}
		})
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
