package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateRejectsRetiredProviderInput is #4879's end-to-end regression:
// the workflow must fail at the real `goobers validate` seam, before a run can
// reach backlog-query's retained runtime defense.
func TestValidateRejectsRetiredProviderInput(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code = %d, stderr = %q", code, stderr)
	}

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	workflow := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@hourly"
  start: query-backlog
  tasks:
    - name: query-backlog
      type: deterministic
      goal: Claim one backlog item.
      capabilities:
        - github:issues:write
      policyActions:
        - claim-backlog-items
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        resultFile: claimed-item.json
        resweepInterval: 6h
      expectedOutputs: [result]
`
	if err := os.WriteFile(workflowPath, []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code == 0 {
		t.Fatalf("validate: code = 0, want retired provider input refusal; stdout = %q, stderr = %q", stdout, stderr)
	}
	for _, want := range []string{
		"ERROR",
		`task "query-backlog"`,
		`retired input "resweepInterval"`,
		"configure schedule and readiness on a separate workflow using backlog-query --claim --resweep",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("validate stdout = %q, want %q", stdout, want)
		}
	}

	jsonCode, jsonOut, jsonErr := runArgs(t, "validate", "--json", root)
	if jsonCode != code || jsonErr != "" {
		t.Fatalf("validate --json: code = %d, stderr = %q, want code %d and empty stderr", jsonCode, jsonErr, code)
	}
	envelope := decodeDiagnosticsEnvelope(t, jsonOut)
	found := false
	for _, finding := range envelope.Findings {
		if finding.Code == "WF026" && strings.Contains(finding.Message, `retired input "resweepInterval"`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("validate --json findings = %+v, want WF026 retired-input finding", envelope.Findings)
	}
}
