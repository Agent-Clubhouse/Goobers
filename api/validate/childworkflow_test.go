package validate

import (
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
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
