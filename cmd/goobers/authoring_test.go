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

func TestExplainEmitsAccurateOrExplicitlyAbsentFacts(t *testing.T) {
	output := runExplainJSON(t, "workflow.spec.gates[].evaluator")
	if output.Type != "string" ||
		!reflect.DeepEqual(output.AllowedValues, []any{"automated", "agentic", "human"}) ||
		output.Required == nil || !*output.Required ||
		!output.Documented {
		t.Fatalf("nested enum explanation = %+v", output)
	}
	if output.Default != nil || output.SinceVersion != "" {
		t.Fatalf("schema-absent facts were emitted: %+v", output)
	}

	capabilities := runExplainJSON(t, "goober.spec.capabilities")
	if capabilities.Type != "array" || len(capabilities.AllowedValues) == 0 ||
		capabilities.AllowedValues[0] != "repo:read" {
		t.Fatalf("capability explanation = %+v", capabilities)
	}

	undocumented := runExplainJSON(t, "features.version")
	if undocumented.Documented || undocumented.Description != "" {
		t.Fatalf("undocumented field = %+v", undocumented)
	}

	code, raw, stderr := runArgs(t, "explain", "workflow.spec.triggers[].labelPredicate")
	if code != 0 || stderr != "" {
		t.Fatalf("explain labelPredicate: code=%d stderr=%q", code, stderr)
	}
	var labelPredicate map[string]any
	if err := json.Unmarshal([]byte(raw), &labelPredicate); err != nil {
		t.Fatal(err)
	}
	if _, ok := labelPredicate["examples"]; ok {
		t.Fatalf("label predicate received an examples field: %s", raw)
	}
	if _, ok := labelPredicate["stability"]; ok {
		t.Fatalf("label predicate received a stability field: %s", raw)
	}
}

func TestAuthoringCommandsWorkWithoutSourceCheckout(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	for _, args := range [][]string{
		{"schema", "workflow"},
		{"schema", "--list"},
		{"explain", "goober.spec.harness"},
	} {
		code, _, stderr := runArgs(t, args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: code=%d stderr=%q", args, code, stderr)
		}
	}
}

func TestAuthoringCommandsRejectUnknownAndInvalidInput(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"schema", "not-a-schema"}, want: `unknown schema kind "not-a-schema"`},
		{args: []string{"explain", "workflow.stages[].gate"}, want: `unknown selector "workflow.stages[].gate"`},
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
