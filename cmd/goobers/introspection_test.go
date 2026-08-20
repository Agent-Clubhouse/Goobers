package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	apivalidate "github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/supportmatrix"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
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

			humanCode, humanStdout, humanStderr := runArgs(t, "validate", root)
			if humanStderr != "" {
				t.Fatalf("validate stderr = %q, want empty", humanStderr)
			}
			humanGolden := filepath.Join("testdata", "introspection", "diagnostics."+tc.name+".human.golden.txt")
			assertGoldenFile(t, humanGolden, humanStdout)

			lintHumanCode, lintHumanStdout, lintHumanStderr := runArgs(t, "lint", root)
			if lintHumanCode != humanCode || lintHumanStdout != humanStdout || lintHumanStderr != humanStderr {
				t.Fatalf("lint and validate human output drifted:\nvalidate code=%d stdout=%q stderr=%q\nlint code=%d stdout=%q stderr=%q",
					humanCode, humanStdout, humanStderr, lintHumanCode, lintHumanStdout, lintHumanStderr)
			}

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

func TestEncodeSchemaJSONRejectsUndeclaredFieldBeforeWrite(t *testing.T) {
	type diagnosticsWithFutureField struct {
		diagnosticsEnvelope
		FutureField string `json:"futureField"`
	}
	var stdout strings.Builder
	err := encodeSchemaJSON(&stdout, schemas.Diagnostics, diagnosticsWithFutureField{
		diagnosticsEnvelope: (&diagnosticCollector{}).envelope(true),
		FutureField:         "not declared in diagnostics.schema.json",
	})
	if err == nil {
		t.Fatal("schema-invalid diagnostics envelope was accepted")
	}
	if stdout.Len() != 0 {
		t.Fatalf("schema-invalid envelope wrote %q before validation", stdout.String())
	}
}

func TestValidateJSONMultiDocumentYAMLLocation(t *testing.T) {
	root := initIntrospectionInstance(t)
	path := defaultWorkflowPath(root)
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	malformed := append(valid, []byte("---\napiVersion: goobers.dev/v1alpha1\nkind: Workflow\n\tbad: true\n")...)
	if err := os.WriteFile(path, malformed, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", "--json", root)
	if code != 1 || stderr != "" {
		t.Fatalf("validate multi-document YAML: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	envelope := decodeDiagnosticsEnvelope(t, stdout)
	wantFile := filepath.ToSlash(filepath.Join("config", "gaggles", "example", "workflows", "default-implement.yaml"))
	wantLine := strings.Count(string(valid), "\n") + 4
	const wantMessage = "invalid YAML: yaml: line 4: found a tab character that violates indentation"
	found := false
	for _, finding := range envelope.Findings {
		if finding.Code != "YAML001" {
			continue
		}
		if finding.File != wantFile || finding.Line != wantLine || finding.Col != 1 || finding.Message != wantMessage {
			t.Fatalf("invalid-YAML finding = %+v, want file=%q line=%d col=1 message=%q",
				finding, wantFile, wantLine, wantMessage)
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("YAML001 finding not found in %+v", envelope.Findings)
	}

	humanCode, humanStdout, humanStderr := runArgs(t, "validate", root)
	if humanCode != code || humanStderr != "" {
		t.Fatalf("human validate multi-document YAML: code=%d stderr=%q stdout=%q",
			humanCode, humanStderr, humanStdout)
	}
	wantHuman := "ERROR   " +
		filepath.ToSlash(filepath.Join("gaggles", "example", "workflows", "default-implement.yaml")) +
		": " + wantMessage + "\n\nconfig directory failed validation\n"
	if humanStdout != wantHuman {
		t.Fatalf("human output = %q, want byte-for-byte %q", humanStdout, wantHuman)
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
		assertGoldenFile(t, filepath.Join("testdata", "introspection", "features.all.golden.json"), stdout)
		for _, feature := range envelope.Features {
			if feature.Used != nil {
				t.Fatalf("unfiltered feature %q unexpectedly carries used", feature.Name)
			}
		}
		humanCode, humanStdout, humanStderr := runArgs(t, "features")
		if humanCode != code || humanStderr != "" {
			t.Fatalf("features human output: code=%d stderr=%q", humanCode, humanStderr)
		}
		assertGoldenFile(t, filepath.Join("testdata", "introspection", "features.all.human.golden.txt"), humanStdout)
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
		assertGoldenFile(t, filepath.Join("testdata", "introspection", "features.dsl-version.golden.json"), stdout)
		humanCode, humanStdout, humanStderr := runArgs(t, "features", "--dsl-version", supportmatrix.CurrentDSLVersion)
		if humanCode != code || humanStderr != "" {
			t.Fatalf("features --dsl-version human output: code=%d stderr=%q", humanCode, humanStderr)
		}
		assertGoldenFile(t, filepath.Join("testdata", "introspection", "features.dsl-version.human.golden.txt"), humanStdout)
	})

	t.Run("used", func(t *testing.T) {
		root := initIntrospectionInstance(t)
		code, stdout, stderr := runArgs(t, "features", "--json", "--used", root)
		if code != 0 || stderr != "" {
			t.Fatalf("features --json --used code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		envelope := decodeFeaturesEnvelope(t, stdout)
		assertFeaturesSchema(t, stdout)
		assertGoldenFile(t, filepath.Join("testdata", "introspection", "features.used.golden.json"), stdout)
		if len(envelope.Features) == 0 {
			t.Fatal("used feature list is empty")
		}
		for _, feature := range envelope.Features {
			if feature.Used == nil || !*feature.Used {
				t.Fatalf("used feature %q does not carry used=true", feature.Name)
			}
		}
		humanCode, humanStdout, humanStderr := runArgs(t, "features", "--used", root)
		if humanCode != code || humanStderr != "" {
			t.Fatalf("features --used human output: code=%d stderr=%q", humanCode, humanStderr)
		}
		assertGoldenFile(t, filepath.Join("testdata", "introspection", "features.used.human.golden.txt"), humanStdout)
	})
}

func TestValidateJSONLateChecksUseDefinitionSources(t *testing.T) {
	t.Run("compile", func(t *testing.T) {
		root := initIntrospectionInstance(t)
		path := defaultWorkflowPath(root)
		replaceInFile(t, path,
			"      expectedOutputs:\n        - pull-request-url\n        - prNumber\n        - opened",
			"      expectedOutputs:\n        - pull-request-url\n        - prNumber\n        - opened\n      next: verify\n  gates:\n    - name: verify\n      evaluator: automated\n      automated:\n        check: missing-check\n      branches:\n        pass: \"\"\n        fail: \"@abort\"\n        escalate: \"@abort\"")

		code, stdout, stderr := runArgs(t, "validate", "--json", root)
		if code != 1 || stderr != "" {
			t.Fatalf("validate compile diagnostic: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertFindingSource(t, decodeDiagnosticsEnvelope(t, stdout), "COMPILE001",
			filepath.ToSlash(filepath.Join("config", "gaggles", "example", "workflows", "default-implement.yaml")), "/")
	})

	t.Run("docs root", func(t *testing.T) {
		root := initIntrospectionInstance(t)
		runGitT(t, root, "init", "-q")
		// #3285: the existence ERROR only fires when the validated tree is a
		// checkout of the gaggle's target repository (starter spec.project =
		// your-org/your-repo); without this remote it is an advisory warning.
		runGitT(t, root, "remote", "add", "origin", "https://github.com/your-org/your-repo.git")
		replaceInFile(t, defaultWorkflowPath(root), "  start: query-backlog",
			"  start: query-backlog\n  docsRoots:\n    - missing-docs")

		code, stdout, stderr := runArgs(t, "validate", "--json", root)
		if code != 1 || stderr != "" {
			t.Fatalf("validate docs-root diagnostic: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertFindingSource(t, decodeDiagnosticsEnvelope(t, stdout), "DOCS002",
			filepath.ToSlash(filepath.Join("config", "gaggles", "example", "workflows", "default-implement.yaml")),
			"/spec/docsRoots/0")
	})

	t.Run("stage command", func(t *testing.T) {
		// A bare `goobers` with no verb: the one unknown-command shape the
		// DSL compilers' admission check (WF010, C+D2/#2861 wave) does not
		// cover, so it still reaches the late #650 CLI-surface pass whose
		// definition-source attribution this test pins. (An unknown VERB is
		// now rejected earlier, during api/validate — see the
		// "stage command verb" case below.)
		root := initIntrospectionInstance(t)
		replaceInFile(t, defaultWorkflowPath(root),
			`command: ["goobers", "backlog-query", "--claim"]`,
			`command: ["goobers"]`)

		code, stdout, stderr := runArgs(t, "validate", "--json", root)
		if code != 1 || stderr != "" {
			t.Fatalf("validate stage-command diagnostic: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertFindingSource(t, decodeDiagnosticsEnvelope(t, stdout), "COMMAND001",
			filepath.ToSlash(filepath.Join("config", "gaggles", "example", "workflows", "default-implement.yaml")),
			"/spec/tasks/0/run/command")
	})

	t.Run("stage command verb", func(t *testing.T) {
		root := initIntrospectionInstance(t)
		replaceInFile(t, defaultWorkflowPath(root), `"backlog-query"`, `"missing-command"`)

		code, stdout, stderr := runArgs(t, "validate", "--json", root)
		if code != 1 || stderr != "" {
			t.Fatalf("validate stage-command-verb diagnostic: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertFindingSource(t, decodeDiagnosticsEnvelope(t, stdout), "WF010",
			filepath.ToSlash(filepath.Join("config", "gaggles", "example", "workflows", "default-implement.yaml")),
			"/spec/tasks")
	})

	t.Run("mcp config", func(t *testing.T) {
		root := initIntrospectionInstance(t)
		path := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, []byte(`
  mcpServers:
    - name: context
      command: context-server
    - name: context
      command: other-context-server
`)...)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}

		code, stdout, stderr := runArgs(t, "validate", "--json", root)
		if code != 1 || stderr != "" {
			t.Fatalf("validate MCP diagnostic: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertFindingSource(t, decodeDiagnosticsEnvelope(t, stdout), "MCP001",
			filepath.ToSlash(filepath.Join("config", "gaggles", "example", "goobers", "coder", "goober.yaml")),
			"/spec/mcpServers/1/name")
	})

	t.Run("harness", func(t *testing.T) {
		root := initIntrospectionInstance(t)
		withHarnessAdapter(t, func(apiv1.Harness, []string, map[string][]string) (harness.Adapter, error) {
			return &harnesstest.FakeAdapter{PreflightErr: errNotSignedIn}, nil
		})

		code, stdout, stderr := runArgs(t, "validate", "--json", "--check-harness", root)
		if code != 1 || stderr != "" {
			t.Fatalf("validate harness diagnostic: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertFindingSource(t, decodeDiagnosticsEnvelope(t, stdout), "HARNESS003",
			filepath.ToSlash(filepath.Join("config", "gaggles", "example", "goobers", "coder", "goober.yaml")),
			"/spec/harness")
	})
}

func assertFindingSource(t *testing.T, envelope diagnosticsEnvelope, code, file, path string) {
	t.Helper()
	for _, finding := range envelope.Findings {
		if finding.Code == code {
			if finding.File != file || finding.Path != path {
				t.Fatalf("finding %s source = %q %q, want %q %q", code, finding.File, finding.Path, file, path)
			}
			return
		}
	}
	t.Fatalf("finding %s not found in %+v", code, envelope.Findings)
}

func initIntrospectionInstance(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "instance")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	// The starter scaffold now ships its own gaggle-scoped implement/run-tests
	// skill packages (SKILL002 fix); declaring shared-level stand-ins here as
	// well would collide with them (SKILL001) instead of being a harmless
	// no-op.
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
	if envelope.Counts.Errors+envelope.Counts.Warnings+envelope.Counts.Infos != len(envelope.Findings) {
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
