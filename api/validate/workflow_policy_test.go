package validate

import (
	"strings"
	"testing"
)

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

func TestWorkflowSchemaRejectsSyncBaseInScratchWorkspace(t *testing.T) {
	v := newV(t)
	workflow := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Workflow",
		"metadata": {"name": "sync-base"},
		"spec": {
			"gaggle": "example",
			"triggers": [{"type": "manual"}],
			"start": "local-ci",
			"tasks": [{
				"name": "local-ci",
				"type": "deterministic",
				"goal": "validate",
				"run": {"command": ["make", "ci"], WORKSPACE}
			}]
		}
	}`
	for _, tc := range []struct {
		name      string
		runFields string
		wantErr   bool
	}{
		{name: "default repo", runFields: `"syncBase": true`},
		{name: "explicit repo", runFields: `"workspace": "repo", "syncBase": true`},
		{name: "scratch without sync", runFields: `"workspace": "scratch"`},
		{name: "scratch with sync", runFields: `"workspace": "scratch", "syncBase": true`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := v.ValidateJSON("workflow.schema.json", []byte(strings.Replace(workflow, "WORKSPACE", tc.runFields, 1)))
			if tc.wantErr && err == nil {
				t.Fatal("expected schema validation to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected schema validation to pass, got %v", err)
			}
		})
	}
}
