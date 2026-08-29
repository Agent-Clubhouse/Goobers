package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const humanGateWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: backlog-item
      selector:
        goobers: "true"
  start: approval
  gates:
    - name: approval
      evaluator: human
      human:
        approvers:
          - maintainers
      branches:
        pass: ""
        fail: "@abort"
`

func humanGateInstance(t *testing.T) string {
	t.Helper()
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(humanGateWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestValidateAcceptsHumanGate(t *testing.T) {
	root := humanGateInstance(t)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code = %d, want 0; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "OK: instance.yaml valid; config/ valid") {
		t.Fatalf("validate stdout = %q, want valid human-gate workflow", stdout)
	}
}

func TestDaemonAcceptsHumanGateAtStartup(t *testing.T) {
	root := humanGateInstance(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := runUpContext(ctx, []string{root}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("up: code = %d, want 0; stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "daemon started") {
		t.Fatalf("daemon did not start with human gate: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
