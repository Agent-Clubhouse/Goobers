package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestGateOutcomeCoverageDiagnostics(t *testing.T) {
	tests := []struct {
		name        string
		check       string
		branches    string
		wantMessage string
	}{
		{
			name:  "land-outcome missing enqueued",
			check: "land-outcome",
			branches: `        merged: ""
        fail: "@abort"`,
			wantMessage: `gate "outcome-gate": producible outcome "enqueued" has no branch (would fail closed at evaluation time)`,
		},
		{
			name:  "queue-outcome missing evicted",
			check: "queue-outcome",
			branches: `        merged: ""
        timeout: "@escalate"
        fail: "@abort"`,
			wantMessage: `gate "outcome-gate": producible outcome "evicted" has no branch (would fail closed at evaluation time)`,
		},
		{
			name:  "explicit terminal branches",
			check: "queue-outcome",
			branches: `        merged: ""
        evicted: ""
        timeout: "@escalate"
        fail: "@abort"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: outcome-coverage
spec:
  instance:
    name: outcome-coverage
    environment: dev
  gaggles:
    - web
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: web
spec:
  project:
    provider: github
    owner: acme
    name: web
  backlog:
    provider: github
    project: acme/web
  isolation:
    namespace: gaggle-web
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: outcome-coverage
spec:
  gaggle: web
  triggers:
    - type: manual
  start: outcome-gate
  gates:
    - name: outcome-gate
      evaluator: automated
      automated:
        check: %s
      branches:
%s
`, tc.check, tc.branches)

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			var errors []string
			for _, issue := range report.Issues {
				if issue.Severity == Error {
					errors = append(errors, issue.Message)
				}
			}
			if tc.wantMessage == "" {
				if len(errors) != 0 {
					t.Fatalf("explicit terminal branches should validate, got errors: %v", errors)
				}
				return
			}
			if len(errors) != 1 || errors[0] != tc.wantMessage {
				t.Fatalf("validation errors = %v, want [%q]", errors, tc.wantMessage)
			}
		})
	}
}
