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
	report := validateDSL30(t, dsl30Config(true, workflow))
	var found bool
	for _, issue := range report.Issues {
		if issue.Severity == Error && strings.Contains(issue.Message, `artifact input "telemetry" references unknown artifact slot "missing" on producer "produce"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues = %+v, want unknown artifact slot error", report.Issues)
	}
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
