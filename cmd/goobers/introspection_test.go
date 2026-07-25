package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	apivalidate "github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestDiagnosticsJSONGolden(t *testing.T) {
	type scenario struct {
		name   string
		mutate func(t *testing.T, root string)
	}
	scenarios := []scenario{
		{name: "clean"},
		{
			name: "single-finding",
			mutate: func(t *testing.T, root string) {
				replaceInFile(t, defaultWorkflowPath(root), "  start: query-backlog", "  start: missing")
			},
		},
		{
			name: "multi-finding",
			mutate: func(t *testing.T, root string) {
				path := defaultWorkflowPath(root)
				replaceInFile(t, path, "  start: query-backlog", "  start: missing")
				replaceInFile(t, path, "      next: implement", "      next: also-missing")
			},
		},
		{
			name: "multi-file",
			mutate: func(t *testing.T, root string) {
				replaceInFile(t, defaultWorkflowPath(root), "  start: query-backlog", "  start: missing")
				replaceInFile(t, filepath.Join(root, "config", "manifest.yaml"), "    - example", "    - missing")
			},
		},
	}

	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			root := initIntrospectionInstance(t)
			if tc.mutate != nil {
				tc.mutate(t, root)
			}

			humanCode, _, _ := runArgs(t, "validate", root)
			jsonCode, stdout, stderr := runArgs(t, "validate", "--json", root)
			if jsonCode != humanCode {
				t.Fatalf("validate exit code changed with --json: human=%d json=%d", humanCode, jsonCode)
			}
			if stderr != "" {
				t.Fatalf("validate --json stderr = %q, want empty", stderr)
			}
			envelope := decodeDiagnosticsEnvelope(t, stdout)
			assertDiagnosticsSchema(t, stdout)
			assertDiagnosticLocations(t, envelope.Findings)

			golden := filepath.Join("testdata", "introspection", "diagnostics."+tc.name+".golden.json")
			assertGoldenFile(t, golden, stdout)

			lintCode, lintStdout, lintStderr := runArgs(t, "lint", "--json", root)
			if lintCode != jsonCode {
				t.Fatalf("lint --json code=%d, validate --json code=%d", lintCode, jsonCode)
			}
			if lintStdout != stdout || lintStderr != stderr {
				t.Fatalf("lint and validate JSON drifted:\nvalidate stdout:\n%s\nlint stdout:\n%s\nvalidate stderr=%q lint stderr=%q",
					stdout, lintStdout, stderr, lintStderr)
			}
		})
	}
}

func TestValidateJSONRoundTripRepair(t *testing.T) {
	root := initIntrospectionInstance(t)
	path := defaultWorkflowPath(root)
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(valid, []byte("\nbroken: [\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", "--json", root)
	if code != 1 || stderr != "" {
		t.Fatalf("invalid validate: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	envelope := decodeDiagnosticsEnvelope(t, stdout)
	if envelope.OK || len(envelope.Findings) == 0 {
		t.Fatalf("invalid config envelope = %+v", envelope)
	}
	wantFile := filepath.ToSlash(filepath.Join("config", "gaggles", "example", "workflows", "default-implement.yaml"))
	var located *diagnosticFinding
	for i := range envelope.Findings {
		if envelope.Findings[i].File == wantFile && strings.Contains(envelope.Findings[i].Message, "invalid YAML") {
			located = &envelope.Findings[i]
			break
		}
	}
	if located == nil {
		t.Fatalf("no invalid-YAML finding for %q: %+v", wantFile, envelope.Findings)
	}
	if located.Line == 0 && located.Path == "" {
		t.Fatalf("finding has no source location: %+v", located)
	}

	if err := os.WriteFile(path, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runArgs(t, "validate", "--json", root)
	if code != 0 || stderr != "" {
		t.Fatalf("repaired validate: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	repaired := decodeDiagnosticsEnvelope(t, stdout)
	if !repaired.OK || repaired.Counts.Errors != 0 {
		t.Fatalf("repaired config envelope = %+v", repaired)
	}
}

func TestFeaturesJSONContract(t *testing.T) {
	t.Run("all", func(t *testing.T) {
		code, stdout, stderr := runArgs(t, "features", "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("features --json: code=%d stderr=%q", code, stderr)
		}
		envelope := decodeFeaturesEnvelope(t, stdout)
		assertFeaturesSchema(t, stdout)
		if envelope.DSLVersion != "all" || len(envelope.Features) == 0 {
			t.Fatalf("features envelope = %+v", envelope)
		}
		for _, feature := range envelope.Features {
			if feature.Used != nil {
				t.Fatalf("unfiltered feature %q unexpectedly carries used", feature.Name)
			}
		}
	})

	t.Run("dsl-version", func(t *testing.T) {
		code, stdout, stderr := runArgs(t, "features", "--json", "--dsl-version", supportmatrix.CurrentDSLVersion)
		if code != 0 || stderr != "" {
			t.Fatalf("features --json --dsl-version: code=%d stderr=%q", code, stderr)
		}
		envelope := decodeFeaturesEnvelope(t, stdout)
		assertFeaturesSchema(t, stdout)
		if envelope.DSLVersion != supportmatrix.CurrentDSLVersion {
			t.Fatalf("dslVersion = %q, want %q", envelope.DSLVersion, supportmatrix.CurrentDSLVersion)
		}
	})

	t.Run("used", func(t *testing.T) {
		root := initIntrospectionInstance(t)
		code, stdout, _ := runArgs(t, "features", "--json", "--used", root)
		if code != 0 {
			t.Fatalf("features --json --used code=%d stdout=%q", code, stdout)
		}
		envelope := decodeFeaturesEnvelope(t, stdout)
		assertFeaturesSchema(t, stdout)
		if len(envelope.Features) == 0 {
			t.Fatal("used feature list is empty")
		}
		for _, feature := range envelope.Features {
			if feature.Used == nil || !*feature.Used {
				t.Fatalf("used feature %q does not carry used=true", feature.Name)
			}
		}
	})
}

func initIntrospectionInstance(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "instance")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	replaceInFile(t, defaultWorkflowPath(root),
		"    - type: manual",
		"    - type: schedule\n      schedule: \"@hourly\"")
	return root
}

func defaultWorkflowPath(root string) string {
	return filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
}

func decodeDiagnosticsEnvelope(t *testing.T, raw string) diagnosticsEnvelope {
	t.Helper()
	var envelope diagnosticsEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode diagnostics JSON: %v\n%s", err, raw)
	}
	if envelope.SchemaVersion != diagnosticsSchemaVersion || envelope.Version == "" || envelope.Commit == "" {
		t.Fatalf("diagnostics identity is incomplete: %+v", envelope)
	}
	if envelope.Counts.Errors+envelope.Counts.Warnings != len(envelope.Findings) {
		t.Fatalf("counts %+v do not match %d findings", envelope.Counts, len(envelope.Findings))
	}
	return envelope
}

func decodeFeaturesEnvelope(t *testing.T, raw string) featuresEnvelope {
	t.Helper()
	var envelope featuresEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode features JSON: %v\n%s", err, raw)
	}
	if envelope.SchemaVersion != featuresSchemaVersion || envelope.Version == "" || envelope.Commit == "" {
		t.Fatalf("features identity is incomplete: %+v", envelope)
	}
	return envelope
}

func assertDiagnosticLocations(t *testing.T, findings []diagnosticFinding) {
	t.Helper()
	for _, finding := range findings {
		if finding.File == "" || finding.Code == "" || finding.Message == "" {
			t.Errorf("incomplete finding: %+v", finding)
		}
		if finding.Path == "" && (finding.Line == 0 || finding.Col == 0) {
			t.Errorf("finding has neither structured path nor line/column: %+v", finding)
		}
	}
}

func assertDiagnosticsSchema(t *testing.T, raw string) {
	t.Helper()
	assertIntrospectionSchema(t, schemas.Diagnostics, raw)
}

func assertFeaturesSchema(t *testing.T, raw string) {
	t.Helper()
	assertIntrospectionSchema(t, schemas.Features, raw)
}

func assertIntrospectionSchema(t *testing.T, schemaFile, raw string) {
	t.Helper()
	validator, err := apivalidate.New()
	if err != nil {
		t.Fatalf("build schema validator: %v", err)
	}
	if err := validator.ValidateJSON(schemaFile, []byte(raw)); err != nil {
		t.Fatalf("%s validation: %v\n%s", schemaFile, err, raw)
	}
}

func assertGoldenFile(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("output differs from %s; regenerate with UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestDiagnosticsJSONGolden", path)
	}
}

func TestDiagnosticPath(t *testing.T) {
	tests := map[string]string{
		"/spec/tasks/0/name: missing property":                "/spec/tasks/0/name",
		`spec.additionalRepos[2].connectionRef is unresolved`: "/spec/additionalRepos/2/connectionRef",
		"document-level error":                                "/",
	}
	for message, want := range tests {
		if got := diagnosticPath(message); got != want {
			t.Errorf("diagnosticPath(%q) = %q, want %q", message, got, want)
		}
	}
	if line, col := diagnosticLineCol("yaml: line 17: bad value"); line != 17 || col != 0 {
		t.Errorf("diagnosticLineCol() = %d,%d, want 17,0", line, col)
	}
	if strings.TrimSpace(diagnosticsSchemaVersion) == "" || strings.TrimSpace(featuresSchemaVersion) == "" {
		t.Fatal("schema versions must not be empty")
	}
}
