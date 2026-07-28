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

func TestSchemaListsEveryEmbeddedKind(t *testing.T) {
	code, stdout, stderr := runArgs(t, "schema", "--list")
	if code != 0 || stderr != "" {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var output schemaListOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if !reflect.DeepEqual(output.Kinds, schemas.Kinds()) {
		t.Fatalf("kinds = %v, want %v", output.Kinds, schemas.Kinds())
	}
	assertAuthoringStamp(t, output.authoringStamp, schemaOutputVersion)
}

func TestSchemaOutputPreservesEveryEmbeddedSchemaByteForByte(t *testing.T) {
	const marker = `,"schema":`
	for _, entry := range schemas.Entries() {
		t.Run(entry.Kind, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("..", "..", "api", "schemas", entry.File))
			if err != nil {
				t.Fatal(err)
			}
			embedded, err := schemas.FS.ReadFile(entry.File)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(embedded, want) {
				t.Fatalf("embedded %s differs from api/schemas source", entry.File)
			}

			code, stdout, stderr := runArgs(t, "schema", entry.Kind)
			if code != 0 || stderr != "" {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			start := strings.Index(stdout, marker)
			if start < 0 || !strings.HasSuffix(stdout, "}\n") {
				t.Fatalf("schema envelope does not contain a schema member:\n%s", stdout)
			}
			got := []byte(stdout[start+len(marker) : len(stdout)-2])
			if !bytes.Equal(got, want) {
				t.Fatalf("schema %q output is not byte-identical to %s", entry.Kind, entry.File)
			}

			var decoded struct {
				authoringStamp
				Kind   string          `json:"kind"`
				Schema json.RawMessage `json:"schema"`
			}
			if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
				t.Fatalf("decode schema envelope: %v", err)
			}
			if decoded.Kind != entry.Kind {
				t.Errorf("kind = %q, want %q", decoded.Kind, entry.Kind)
			}
			assertAuthoringStamp(t, decoded.authoringStamp, schemaOutputVersion)
		})
	}
}

func TestAuthoringOutputsMatchPublishedSchemas(t *testing.T) {
	validator, err := apivalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		schemaFile string
		args       []string
	}{
		{name: "schema document", schemaFile: schemas.SchemaOutput, args: []string{"schema", "goober"}},
		{name: "schema list", schemaFile: schemas.SchemaOutput, args: []string{"schema", "--list"}},
		{name: "documented explanation", schemaFile: schemas.ExplainOutput, args: []string{"explain", "goober.spec.capabilities"}},
		{name: "undocumented explanation", schemaFile: schemas.ExplainOutput, args: []string{"explain", "features.version"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, test.args...)
			if code != 0 || stderr != "" {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			if err := validator.ValidateJSON(test.schemaFile, []byte(stdout)); err != nil {
				t.Fatalf("output does not match %s: %v\n%s", test.schemaFile, err, stdout)
			}
		})
	}
}

func TestExplainEmitsCompleteAuthoringGuidance(t *testing.T) {
	output := runExplainJSON(t, "workflow.spec.gates[].evaluator")
	if output.Type != "string" ||
		!reflect.DeepEqual(output.AllowedValues, []any{"automated", "agentic", "human"}) ||
		output.Required == nil || !*output.Required ||
		!output.Documented ||
		output.Stability != "ga" ||
		output.SinceVersion == "" ||
		output.Example != "automated" {
		t.Fatalf("nested enum explanation = %+v", output)
	}
	if output.Default != nil {
		t.Fatalf("schema-absent default was emitted: %+v", output)
	}

	capabilities := runExplainJSON(t, "goober.spec.capabilities")
	if capabilities.Type != "array" || len(capabilities.AllowedValues) == 0 ||
		capabilities.AllowedValues[0] != "repo:read" ||
		!reflect.DeepEqual(capabilities.Example, []any{"repo:read"}) {
		t.Fatalf("capability explanation = %+v", capabilities)
	}

	constField := runExplainJSON(t, "goober.apiVersion")
	if constField.Type != "string" ||
		constField.Example != "goobers.dev/v1alpha1" {
		t.Fatalf("const field explanation = %+v", constField)
	}

	versionGated := runExplainJSON(t, "gaggle.spec.sandbox")
	if versionGated.Stability != "preview" || versionGated.SinceVersion == "" ||
		versionGated.Example == nil {
		t.Fatalf("version-gated explanation = %+v", versionGated)
	}
}

func TestAuthoringCommandsAuthorAndValidateWithoutSourceCheckout(t *testing.T) {
	root := initDemo(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	assertSchemaRequired(t, "goober", nil, "apiVersion", "kind", "metadata", "spec")
	assertSchemaRequired(t, "goober", []string{"spec"}, "gaggle", "role", "instructions")
	assertSchemaRequired(t, "workflow", nil, "apiVersion", "kind", "metadata", "spec")
	assertSchemaRequired(t, "workflow", []string{"spec"}, "gaggle", "triggers", "start")

	gooberName := explainString(t, "goober.metadata.name")
	instructions := explainString(t, "goober.spec.instructions")
	goober := map[string]any{
		"apiVersion": explainString(t, "goober.apiVersion"),
		"kind":       explainString(t, "goober.kind"),
		"metadata": map[string]any{
			"name": gooberName,
		},
		"spec": map[string]any{
			"gaggle":       explainString(t, "goober.spec.gaggle"),
			"role":         explainString(t, "goober.spec.role"),
			"instructions": instructions,
			"harness":      explainString(t, "goober.spec.harness"),
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

	workflowName := explainString(t, "workflow.metadata.name")
	workflowDocument := map[string]any{
		"apiVersion": explainString(t, "workflow.apiVersion"),
		"kind":       explainString(t, "workflow.kind"),
		"dslVersion": explainString(t, "workflow.dslVersion"),
		"metadata": map[string]any{
			"name": workflowName,
		},
		"spec": map[string]any{
			"gaggle":   explainString(t, "workflow.spec.gaggle"),
			"triggers": []any{explainObject(t, "workflow.spec.triggers[]")},
			"start":    explainString(t, "workflow.spec.start"),
			"tasks": []any{map[string]any{
				"name": explainString(t, "workflow.spec.tasks[].name"),
				"type": explainString(t, "workflow.spec.tasks[].type"),
				"goal": explainString(t, "workflow.spec.tasks[].goal"),
				"run": map[string]any{
					"command": explainStringSlice(t, "workflow.spec.tasks[].run.command"),
				},
			}},
		},
	}
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", workflowName+".yaml")
	writeJSONDocument(t, workflowPath, workflowDocument)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate offline-authored definitions: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "2 goober(s), 2 workflow(s)") {
		t.Fatalf("offline-authored definitions were not loaded: %q", stdout)
	}
}

func TestAuthoringCommandsRejectUnknownAndInvalidInput(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"schema", "not-a-schema"}, want: `unknown schema kind "not-a-schema"`},
		{args: []string{"explain", "workflow.stages[].gate"}, want: `unknown selector "workflow.stages[].gate"`},
		{args: []string{"explain", "workflow.spec.tasks[].run.script"}, want: `unavailable selector "workflow.spec.tasks[].run.script" in built-in DSL version 1.4`},
	} {
		code, stdout, stderr := runArgs(t, test.args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, test.want) {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", test.args, code, stdout, stderr)
		}
	}
	for _, args := range [][]string{
		{"schema"},
		{"schema", "--list", "goober"},
		{"explain"},
		{"explain", "goober.spec", "workflow.spec"},
	} {
		code, stdout, stderr := runArgs(t, args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "Usage:") {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
}

func TestAuthoringHumanOutput(t *testing.T) {
	code, stdout, stderr := runArgs(t, "schema", "--human", "goober")
	if code != 0 || stderr != "" ||
		!strings.Contains(stdout, "Schema: goober") ||
		!strings.Contains(stdout, `"title": "Goober"`) {
		t.Fatalf("schema --human: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = runArgs(t, "explain", "--human", "workflow.spec.gates[].evaluator")
	if code != 0 || stderr != "" ||
		!strings.Contains(stdout, "Documented:") ||
		!strings.Contains(stdout, `["automated","agentic","human"]`) ||
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
		t.Fatalf("decode explain %q: %v\n%s", selector, err, stdout)
	}
	assertAuthoringStamp(t, output.authoringStamp, explainOutputVersion)
	return output
}

func assertSchemaRequired(t *testing.T, kind string, path []string, fields ...string) {
	t.Helper()
	code, stdout, stderr := runArgs(t, "schema", kind)
	if code != 0 || stderr != "" {
		t.Fatalf("schema %q: code=%d stderr=%q", kind, code, stderr)
	}
	var output struct {
		Schema map[string]any `json:"schema"`
	}
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatal(err)
	}
	node := output.Schema
	for _, field := range path {
		properties, ok := node["properties"].(map[string]any)
		if !ok {
			t.Fatalf("schema %q path %v has no properties", kind, path)
		}
		child, ok := properties[field].(map[string]any)
		if !ok {
			t.Fatalf("schema %q path %v has no field %q", kind, path, field)
		}
		node = child
	}
	required, ok := node["required"].([]any)
	if !ok {
		t.Fatalf("schema %q path %v has no required fields", kind, path)
	}
	for _, field := range fields {
		found := false
		for _, value := range required {
			if value == field {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("schema %q path %v does not require %q", kind, path, field)
		}
	}
}

func explainString(t *testing.T, selector string) string {
	t.Helper()
	value, ok := runExplainJSON(t, selector).Example.(string)
	if !ok || value == "" {
		t.Fatalf("explain %q example is not a non-empty string", selector)
	}
	return value
}

func explainObject(t *testing.T, selector string) map[string]any {
	t.Helper()
	value, ok := runExplainJSON(t, selector).Example.(map[string]any)
	if !ok {
		t.Fatalf("explain %q example is not an object", selector)
	}
	return value
}

func explainStringSlice(t *testing.T, selector string) []any {
	t.Helper()
	value, ok := runExplainJSON(t, selector).Example.([]any)
	if !ok || len(value) == 0 {
		t.Fatalf("explain %q example is not a non-empty array", selector)
	}
	for _, item := range value {
		if _, ok := item.(string); !ok {
			t.Fatalf("explain %q example contains a non-string value", selector)
		}
	}
	return value
}

func writeJSONDocument(t *testing.T, path string, document map[string]any) {
	t.Helper()
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertAuthoringStamp(t *testing.T, stamp authoringStamp, schemaVersion string) {
	t.Helper()
	info := buildversion.Get()
	if stamp.SchemaVersion != schemaVersion {
		t.Errorf("schemaVersion = %q, want %q", stamp.SchemaVersion, schemaVersion)
	}
	if stamp.Version != info.Version || stamp.Commit != info.Commit {
		t.Errorf("build stamp = %s/%s, want %s/%s", stamp.Version, stamp.Commit, info.Version, info.Commit)
	}
	if stamp.DSLVersion != supportmatrix.CurrentDSLVersion {
		t.Errorf("dslVersion = %q, want %q", stamp.DSLVersion, supportmatrix.CurrentDSLVersion)
	}
}
