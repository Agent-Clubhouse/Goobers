package validate

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const childPolicyWorkflow = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: child-parent
spec:
  gaggle: web
  triggers: [{type: manual}]
  start: parent
  tasks:
    - name: parent
      type: agentic
      goal: Delegate work
      goober: coder
      childWorkflows:
        allowedGoobers: [coder]
        allowedCapabilities: [repo:read]
        maxChildren: 4
`

func TestChildWorkflowSchemaAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, old, new       string
		schemaOK, semanticOK bool
	}{
		{name: "valid", schemaOK: true, semanticOK: true},
		{"omitted maximum", "        maxChildren: 4\n", "", true, true},
		{"empty grants", "[repo:read]", "[]", true, true},
		{"empty names", "[coder]", "[]", false, false},
		{"unknown goober", "[coder]", "[unknown]", true, false},
		{"unknown capability", "[repo:read]", "[repo:teleport]", false, false},
		{"runner capability", "[repo:read]", "[configrepo:read]", false, false},
		{"zero maximum", "maxChildren: 4", "maxChildren: 0", false, false},
		{"over maximum", "maxChildren: 4", "maxChildren: 33", false, false},
		{"duplicates", "[coder]", "[coder, coder]", false, false},
		{"unknown field", "maxChildren: 4", "maxChildren: 4\n        bypass: true", false, false},
		{"old DSL", "\"3.1\"", "\"3.0\"", false, false},
		{"agent profile", "maxChildren: 4", "allowedAgentProfiles: [{name: fast, goober: coder, harness: copilot, model: configured-model}]", true, true},
		{"profile unknown field", "maxChildren: 4", "allowedAgentProfiles: [{name: fast, goober: coder, harness: copilot, model: configured-model, flags: bypass}]", false, false},
		{"profile missing model", "maxChildren: 4", "allowedAgentProfiles: [{name: fast, goober: coder, harness: copilot}]", false, false},
		{"profile unlisted goober", "maxChildren: 4", "allowedAgentProfiles: [{name: fast, goober: other, harness: copilot, model: configured-model}]", true, false},
		{"named base", "maxChildren: 4", "allowedBases: [{name: integration, ref: refs/heads/integration}]", true, true},
		{"base expression", "maxChildren: 4", "allowedBases: [{name: integration, ref: 'refs/heads/main~1'}]", true, false},
		{"base short ref", "maxChildren: 4", "allowedBases: [{name: integration, ref: main}]", false, false},
		{"base unknown field", "maxChildren: 4", "allowedBases: [{name: integration, ref: refs/heads/main, url: foreign}]", false, false},
		{"deterministic", "type: agentic", "type: deterministic\n      run: {command: [true]}", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := childPolicyWorkflow
			if tc.old != "" {
				doc = strings.Replace(doc, tc.old, tc.new, 1)
			}
			raw, err := yaml.YAMLToJSON([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			err = newV(t).ValidateJSON("workflow.schema.json", raw)
			if (err == nil) != tc.schemaOK {
				t.Fatalf("schema=%v want accepted=%v", err, tc.schemaOK)
			}
			report := validateDSL30(t, dsl30Config(true, doc))
			hasError := false
			for _, issue := range report.Issues {
				if issue.Severity == Error {
					hasError = true
				}
			}
			if hasError == tc.semanticOK {
				t.Fatalf("semantic validation accepted=%v; issues=%+v", !hasError, report.Issues)
			}
		})
	}
}
