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

// Reproduces the cold-start probes: every flavor reached for `goobers schema instance` and
// `goobers explain instance.repos` on the first file `goobers init` tells them
// to edit, and got `unknown schema kind "instance"` /
// `unknown selector "instance.repos"` — the only major object with no
// introspection. Both now answer.
func TestSchemaAndExplainIntrospectInstanceConfig(t *testing.T) {
	code, stdout, stderr := runArgs(t, "schema", "instance")
	if code != 0 || stderr != "" {
		t.Fatalf("schema instance: code=%d stderr=%q", code, stderr)
	}
	var document struct {
		Kind   string `json:"kind"`
		Schema struct {
			Title      string         `json:"title"`
			Required   []string       `json:"required"`
			Properties map[string]any `json:"properties"`
		} `json:"schema"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatal(err)
	}
	if document.Kind != "instance" || document.Schema.Title != "Instance" {
		t.Fatalf("schema instance emitted kind=%q title=%q", document.Kind, document.Schema.Title)
	}
	for _, section := range []string{"repos", "credentials", "telemetry", "engine", "runConditions", "runner"} {
		if _, ok := document.Schema.Properties[section]; !ok {
			t.Errorf("instance schema does not publish %q", section)
		}
	}

	code, stdout, stderr = runArgs(t, "schema", "--list")
	if code != 0 || stderr != "" {
		t.Fatalf("schema --list: code=%d stderr=%q", code, stderr)
	}
	var list schemaListOutput
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, kind := range list.Kinds {
		if kind == "instance" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("schema --list omits instance: %v", list.Kinds)
	}

	for _, selector := range []string{
		"instance.repos",
		"instance.repos[].provider",
		"instance.repos[].project",
		"instance.credentials[].capability",
		"instance.telemetry.retention",
		"instance.runConditions.maxParallelRuns",
		"instance.runner.capabilities",
		"instance.runner.envPassthrough",
		"instance.runner.defaultStageTimeout",
	} {
		explanation := runExplainJSON(t, selector)
		if strings.TrimSpace(explanation.Description) == "" ||
			explanation.Type == nil ||
			explanation.Stability == "" ||
			explanation.SinceVersion == "" {
			t.Errorf("explain %q: incomplete guidance: %+v", selector, explanation)
		}
	}

	code, stdout, stderr = runArgs(t, "explain", "--human", "instance.runner.capabilities")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "never schedules a single run") {
		t.Fatalf("explain --human instance.runner.capabilities: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestAuthoringCommandsSupportSourceFreeValidation(t *testing.T) {
	root := initDemo(t)
	t.Chdir(root)

	goober := authorRequiredDocument(t, "goober")
	gooberSpec := goober["spec"].(map[string]any)
	gooberName := goober["metadata"].(map[string]any)["name"].(string)
	instructions := gooberSpec["instructions"].(string)
	gooberDir := filepath.Join(root, "config", "gaggles", "example", "goobers", gooberName)
	if err := os.MkdirAll(gooberDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONDocument(t, filepath.Join(gooberDir, "goober.yaml"), goober)
	if err := os.WriteFile(filepath.Join(gooberDir, instructions), []byte("# Offline author\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workflow := authorRequiredDocument(t, "workflow")
	workflowSpec := workflow["spec"].(map[string]any)
	workflowName := workflow["metadata"].(map[string]any)["name"].(string)
	workflowSpec["tasks"] = []any{
		runExplainJSON(t, "workflow.spec.tasks[]").Example,
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
		// workflow.spec.tasks[].run.script is 2.0 surface and explains
		// successfully now that resolution spans every loadable DSL version
		// (#3291); internal/authoring's tests pin the positive behavior.
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

func authorRequiredDocument(t *testing.T, kind string) map[string]any {
	t.Helper()
	code, stdout, stderr := runArgs(t, "schema", kind)
	if code != 0 || stderr != "" {
		t.Fatalf("schema %q: code=%d stderr=%q", kind, code, stderr)
	}
	var output struct {
		Schema struct {
			Required []string `json:"required"`
		} `json:"schema"`
	}
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatal(err)
	}
	document := make(map[string]any, len(output.Schema.Required))
	for _, name := range output.Schema.Required {
		document[name] = runExplainJSON(t, kind+"."+name).Example
	}
	return document
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
