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

const humanGateUnsupportedMessage = "human gates ship with durable pause/resume (#168/#465); until then use an automated gate or remove this block"

func humanGateInstance(t *testing.T) string {
	t.Helper()
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(humanGateWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestValidateRejectsHumanGate(t *testing.T) {
	root := humanGateInstance(t)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate: code = %d, want 1; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "INVALID workflow") || !strings.Contains(stdout, humanGateUnsupportedMessage) {
		t.Fatalf("validate stdout = %q, want compile rejection %q", stdout, humanGateUnsupportedMessage)
	}
}

func TestDaemonRejectsHumanGateBeforeStarting(t *testing.T) {
	root := humanGateInstance(t)

	// The daemon must reject the unsupported human gate at startup and return 1.
	// A bounded context is a safety net: if that rejection ever regressed, the
	// daemon would otherwise idle forever here with no per-op deadline (#798) —
	// the timeout turns that into a fast, legible failure instead of a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := runUpContext(ctx, []string{root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("up: code = %d, want 1; stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `compile workflow "default-implement"`) ||
		!strings.Contains(stderr.String(), humanGateUnsupportedMessage) {
		t.Fatalf("up stderr = %q, want compile rejection %q", stderr.String(), humanGateUnsupportedMessage)
	}
	if strings.Contains(stdout.String(), "daemon started") {
		t.Fatalf("daemon started with an unsupported human gate: stdout = %q", stdout.String())
	}
}
