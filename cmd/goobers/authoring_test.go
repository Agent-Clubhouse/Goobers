package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	apivalidate "github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/supportmatrix"
	buildversion "github.com/goobers/goobers/internal/version"
)

func TestSchemaEmitsEveryEmbeddedContractByteForByte(t *testing.T) {
	validator, err := apivalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "schema", "--list")
	if code != 0 || stderr != "" {
		t.Fatalf("schema --list: code=%d stderr=%q", code, stderr)
	}
	var list schemaListOutput
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list.Kinds, schemas.Kinds()) {
		t.Fatalf("kinds = %v, want %v", list.Kinds, schemas.Kinds())
	}
	assertAuthoringStamp(t, list.authoringStamp, schemaOutputVersion)
	if err := validator.ValidateJSON(schemas.SchemaOutput, []byte(stdout)); err != nil {
		t.Fatalf("schema list does not match published output schema: %v", err)
	}

	const marker = `,"schema":`
	for _, entry := range schemas.Entries() {
		t.Run(entry.Kind, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("..", "..", "api", "schemas", entry.File))
			if err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runArgs(t, "schema", entry.Kind)
			if code != 0 || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			start := strings.Index(stdout, marker)
			if start < 0 || !strings.HasSuffix(stdout, "}\n") {
				t.Fatalf("schema envelope has no schema member:\n%s", stdout)
			}
			got := []byte(stdout[start+len(marker) : len(stdout)-2])
			if !bytes.Equal(got, want) {
				t.Fatalf("schema output is not byte-identical to %s", entry.File)
			}
		})
	}
}

func TestExplainEmitsPublishedAuthoringGuidance(t *testing.T) {
	nested := runExplainJSON(t, "workflow.spec.gates[].evaluator")
	if nested.Type != "string" ||
		!reflect.DeepEqual(nested.AllowedValues, []any{"automated", "agentic", "human"}) ||
		nested.Required == nil || !*nested.Required ||
		nested.Example != "automated" {
		t.Fatalf("nested enum explanation = %+v", nested)
	}
	capabilities := runExplainJSON(t, "goober.spec.capabilities")
	if capabilities.Type != "array" || len(capabilities.AllowedValues) == 0 ||
		!reflect.DeepEqual(capabilities.Example, []any{"repo:read"}) {
		t.Fatalf("capability explanation = %+v", capabilities)
	}
	if got := runExplainJSON(t, "gaggle.spec.sandbox"); got.Stability != "preview" {
		t.Fatalf("version-gated explanation = %+v", got)
	}
}

func TestAuthoringCommandsSupportSourceFreeValidation(t *testing.T) {
	root := initDemo(t)
	t.Chdir(root)

	gooberName := explainExample[string](t, "goober.metadata.name")
	instructions := explainExample[string](t, "goober.spec.instructions")
	goober := map[string]any{
		"apiVersion": explainExample[string](t, "goober.apiVersion"),
		"kind":       explainExample[string](t, "goober.kind"),
		"metadata":   map[string]any{"name": gooberName},
		"spec": map[string]any{
			"gaggle":       explainExample[string](t, "goober.spec.gaggle"),
			"role":         explainExample[string](t, "goober.spec.role"),
			"instructions": instructions,
			"harness":      explainExample[string](t, "goober.spec.harness"),
		},
	}
	gooberDir := filepath.Join(root, "config", "gaggles", "example", "goobers", gooberName)
	if err := os.MkdirAll(gooberDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONDocument(t, filepath.Join(gooberDir, "goober.yaml"), goober)
	if err := os.WriteFile(filepath.Join(gooberDir, instructions), []byte("# Offline author\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workflowName := explainExample[string](t, "workflow.metadata.name")
	workflow := map[string]any{
		"apiVersion": explainExample[string](t, "workflow.apiVersion"),
		"kind":       explainExample[string](t, "workflow.kind"),
		"dslVersion": explainExample[string](t, "workflow.dslVersion"),
		"metadata":   map[string]any{"name": workflowName},
		"spec": map[string]any{
			"gaggle":   explainExample[string](t, "workflow.spec.gaggle"),
			"triggers": []any{explainExample[map[string]any](t, "workflow.spec.triggers[]")},
			"start":    explainExample[string](t, "workflow.spec.start"),
			"tasks": []any{map[string]any{
				"name": explainExample[string](t, "workflow.spec.tasks[].name"),
				"type": explainExample[string](t, "workflow.spec.tasks[].type"),
				"goal": explainExample[string](t, "workflow.spec.tasks[].goal"),
				"run": map[string]any{
					"command": explainExample[[]any](t, "workflow.spec.tasks[].run.command"),
				},
			}},
		},
	}
	writeJSONDocument(t, filepath.Join(root, "config", "gaggles", "example", "workflows", workflowName+".yaml"), workflow)
	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 || !strings.Contains(stdout, "2 goober(s), 2 workflow(s)") {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestAuthoringCommandsRejectInvalidInputAndRenderHumanOutput(t *testing.T) {
	tests := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"schema", "not-a-schema"}, 1, `unknown schema kind "not-a-schema"`},
		{[]string{"explain", "workflow.stages[].gate"}, 1, `unknown selector "workflow.stages[].gate"`},
		{[]string{"explain", "workflow.spec.tasks[].run.script"}, 1, "unavailable selector"},
		{[]string{"schema"}, 2, "Usage:"},
		{[]string{"schema", "--list", "goober"}, 2, "Usage:"},
		{[]string{"explain"}, 2, "Usage:"},
	}
	for _, test := range tests {
		code, stdout, stderr := runArgs(t, test.args...)
		if code != test.code || stdout != "" || !strings.Contains(stderr, test.want) {
			t.Errorf("%v: code=%d stdout=%q stderr=%q", test.args, code, stdout, stderr)
		}
	}
	code, stdout, stderr := runArgs(t, "explain", "--human", "workflow.spec.gates[].evaluator")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Allowed values:") ||
		!strings.Contains(stdout, "DSL version:") {
		t.Fatalf("explain --human: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func runExplainJSON(t *testing.T, selector string) explainOutput {
	t.Helper()
	code, stdout, stderr := runArgs(t, "explain", selector)
	if code != 0 || stderr != "" {
		t.Fatalf("explain %q: code=%d stderr=%q", selector, code, stderr)
	}
	var output explainOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatal(err)
	}
	assertAuthoringStamp(t, output.authoringStamp, explainOutputVersion)
	return output
}

func explainExample[T any](t *testing.T, selector string) T {
	t.Helper()
	value, ok := runExplainJSON(t, selector).Example.(T)
	if !ok {
		t.Fatalf("explain %q example has unexpected type", selector)
	}
	return value
}

func writeJSONDocument(t *testing.T, path string, document map[string]any) {
	t.Helper()
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertAuthoringStamp(t *testing.T, stamp authoringStamp, schemaVersion string) {
	t.Helper()
	info := buildversion.Get()
	if stamp.SchemaVersion != schemaVersion ||
		stamp.Version != info.Version ||
		stamp.Commit != info.Commit ||
		stamp.DSLVersion != supportmatrix.CurrentDSLVersion {
		t.Fatalf("authoring stamp = %+v, build = %+v", stamp, info)
	}
}
