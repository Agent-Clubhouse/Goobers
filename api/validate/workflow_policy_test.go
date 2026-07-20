package validate

import "testing"

func TestWorkflowSchemaAcceptsExecutionPolicyFields(t *testing.T) {
	v := newV(t)
	workflow := []byte(`{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "policy-fields"},
		"spec": {
			"gaggle": "example",
			"triggers": [{"type": "manual"}],
			"start": "build",
			"tasks": [{
				"name": "build",
				"type": "deterministic",
				"goal": "build",
				"timeoutSeconds": 30,
				"limits": {"maxTokens": 1000, "maxCostUSD": 1.5},
				"run": {"command": ["make", "build"], "env": {"CI": "true"}},
				"next": "quality"
			}],
			"gates": [{
				"name": "quality",
				"evaluator": "automated",
				"automated": {
					"check": "ci-status",
					"timeoutSeconds": 60,
					"retry": {"maxAttempts": 2, "backoffSeconds": 5},
					"pollIntervalSeconds": 10
				},
				"branches": {"pass": "", "fail": "@abort", "timeout": "@escalate"}
			}]
		}
	}`)
	if err := v.ValidateJSON("workflow.schema.json", workflow); err != nil {
		t.Fatalf("workflow execution policy fields should validate: %v", err)
	}
}
