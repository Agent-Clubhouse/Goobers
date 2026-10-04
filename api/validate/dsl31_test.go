package validate

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const dsl31ArtifactWorkflow = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: artifact-contract
spec:
  gaggle: web
  triggers:
    - type: manual
  start: produce
  tasks:
    - name: produce
      type: deterministic
      goal: Produce named artifacts.
      run:
        command: ["true"]
        workspace: scratch
      artifactSlots:
        - name: _report
          mediaType: application/json
          schemaPath: schemas/report.schema.json
          maxSize: 1048576
        - name: trace
          mediaType: application/x-ndjson
      next: consume
    - name: consume
      type: deterministic
      goal: Consume named artifacts under local names.
      run:
        command: ["true"]
        workspace: scratch
      artifactInputs:
        _summary: {from: produce._report}
        telemetry: {from: produce.trace}
`

func TestValidateDSL31ArtifactSlotsAndInputs(t *testing.T) {
	report := validateDSL30(t, dsl30Config(true, dsl31ArtifactWorkflow))
	for _, issue := range report.Issues {
		if issue.Severity == Error {
			t.Fatalf("DSL 3.1 artifact contract should validate, got error: %+v", issue)
		}
	}
}

func TestValidateDSL31ArtifactInputRequiresDeclaredProducerSlot(t *testing.T) {
	workflow := strings.Replace(dsl31ArtifactWorkflow, "produce.trace", "produce.missing", 1)
	config := dsl30Config(true, workflow)
	report := validateDSL30(t, config)
	assertDSL31ArtifactIssueLocation(t, report, config,
		`artifact input "telemetry" references unknown artifact slot "missing" on producer "produce"`,
		"telemetry: {from: produce.missing}")
}

func TestValidateDSL31ArtifactCompilerDiagnosticsCarrySourceLocations(t *testing.T) {
	tests := []struct {
		name    string
		replace func(string) string
		want    string
		anchor  string
	}{
		{
			name: "unknown producer",
			replace: func(workflow string) string {
				return strings.Replace(workflow, "produce.trace", "missing.trace", 1)
			},
			want:   `artifact input "telemetry" references unknown producer task "missing"`,
			anchor: "telemetry: {from: missing.trace}",
		},
		{
			name: "unreachable producer",
			replace: func(workflow string) string {
				workflow = strings.Replace(workflow, "start: produce", "start: choose", 1)
				gate := `  gates:
    - name: choose
      evaluator: automated
      automated:
        check: ci-status
      branches:
        pass: produce
        fail: consume
        timeout: consume
`
				return strings.Replace(workflow, "  tasks:\n", gate+"  tasks:\n", 1)
			},
			want:   `artifact input "_summary" references producer "produce", but "produce" does not run on every successful path before "consume"`,
			anchor: "_summary: {from: produce._report}",
		},
		{
			name: "media mismatch",
			replace: func(workflow string) string {
				return strings.Replace(workflow, "_summary: {from: produce._report}", "_summary: {from: produce._report, mediaType: text/plain}", 1)
			},
			want:   `artifact input "_summary" expects mediaType "text/plain", but producer "produce" slot "_report" declares "application/json"`,
			anchor: "_summary: {from: produce._report, mediaType: text/plain}",
		},
		{
			name: "schema mismatch",
			replace: func(workflow string) string {
				return strings.Replace(workflow, "_summary: {from: produce._report}", "_summary: {from: produce._report, schemaPath: schemas/other.schema.json}", 1)
			},
			want:   `artifact input "_summary" expects schemaPath "schemas/other.schema.json", but producer "produce" slot "_report" declares "schemas/report.schema.json"`,
			anchor: "_summary: {from: produce._report, schemaPath: schemas/other.schema.json}",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := dsl30Config(true, tc.replace(dsl31ArtifactWorkflow))
			report := validateDSL30(t, config)
			assertDSL31ArtifactIssueLocation(t, report, config, tc.want, tc.anchor)
		})
	}
}

func TestValidateDSL31ArtifactInputsRejectsDuplicateLocalNameWithLocation(t *testing.T) {
	workflow := strings.Replace(dsl31ArtifactWorkflow,
		`        _summary: {from: produce._report}
        telemetry: {from: produce.trace}`,
		`        summary:
          from: produce._report
        summary:
          from: produce.trace`, 1)
	config := dsl30Config(true, workflow)
	report := validateDSL30(t, config)
	issue := findIssueContaining(t, report, `duplicate key "summary"`)
	if issue.Code != errorInvalidYAML {
		t.Fatalf("code = %s, want %s; issue = %+v", issue.Code, errorInvalidYAML, issue)
	}
	assertIssueAtAnchor(t, issue, config, "        summary:\n          from: produce.trace")
}

func TestWorkflowSchemaRejectsArtifactContractBeforeDSL31(t *testing.T) {
	v := newV(t)
	for _, version := range []string{"2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			workflow := strings.Replace(dsl31ArtifactWorkflow, `dslVersion: "3.1"`, `dslVersion: "`+version+`"`, 1)
			jsonDoc, err := yaml.YAMLToJSON([]byte(workflow))
			if err != nil {
				t.Fatal(err)
			}
			if err := v.ValidateJSON("workflow.schema.json", jsonDoc); err == nil {
				t.Fatalf("workflow.schema.json accepted artifactSlots/artifactInputs for DSL %s", version)
			}
		})
	}
}

func assertDSL31ArtifactIssueLocation(t *testing.T, report *Report, config, wantMessage, anchor string) {
	t.Helper()
	issue := findIssueContaining(t, report, wantMessage)
	if issue.Code != errorStageContract {
		t.Fatalf("code = %s, want %s; issue = %+v", issue.Code, errorStageContract, issue)
	}
	assertIssueAtAnchor(t, issue, config, anchor)
}

func findIssueContaining(t *testing.T, report *Report, text string) Issue {
	t.Helper()
	for _, issue := range report.Issues {
		if issue.Severity == Error && strings.Contains(issue.Message, text) {
			return issue
		}
	}
	t.Fatalf("issues = %+v, want error containing %q", report.Issues, text)
	return Issue{}
}

func assertIssueAtAnchor(t *testing.T, issue Issue, config, anchor string) {
	t.Helper()
	wantLine, wantCol := lineColumnOf(t, config, anchor)
	if issue.Line != wantLine || issue.Col != wantCol {
		t.Fatalf("issue position = line %d col %d, want line %d col %d for %q; issue = %+v",
			issue.Line, issue.Col, wantLine, wantCol, anchor, issue)
	}
}

func lineColumnOf(t *testing.T, text, anchor string) (int, int) {
	t.Helper()
	idx := strings.Index(text, anchor)
	if idx < 0 {
		t.Fatalf("anchor %q not found", anchor)
	}
	line := strings.Count(text[:idx], "\n") + 1
	lastNewline := strings.LastIndex(text[:idx], "\n")
	col := idx + 1
	if lastNewline >= 0 {
		col = idx - lastNewline
	}
	return line, col
}
