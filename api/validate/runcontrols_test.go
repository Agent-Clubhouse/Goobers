package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runControlsConfig builds a single-gaggle Manifest+Gaggle+Workflow where each
// definition's runControls YAML block is substituted verbatim ("" omits it) —
// the minimal shape exercising both WF016 surfaces (checkGaggleRunControls and
// checkWorkflow's ValidateWorkflow call).
func runControlsConfig(gaggleControls, workflowControls string) string {
	return fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: budgets
spec:
  instance:
    name: budgets
    environment: dev
  gaggles:
    - acme
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: acme
spec:
  project:
    provider: github
    owner: example
    name: acme
  backlog:
    provider: github
    project: example/acme
  isolation:
    namespace: gaggle-acme
%s---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: sweep
spec:
  gaggle: acme
  triggers:
    - type: manual
  start: noop
%s  tasks:
    - name: noop
      type: deterministic
      goal: Do nothing.
      run:
        command: ["true"]
`, gaggleControls, workflowControls)
}

// TestRunControlsDurationDiagnostics pins the rendered author-facing shape of
// an invalid runControls duration on both definition kinds: the diagnostic
// must carry the full field path and the offending value, and the raw Go
// parse error ("time: invalid duration ...") must not leak through the DSL
// contract — it used to surface verbatim via compile and validate.
func TestRunControlsDurationDiagnostics(t *testing.T) {
	tests := []struct {
		name             string
		gaggleControls   string
		workflowControls string
		want             string // required issue substring when wantErr
		wantErr          bool
	}{
		{
			name:             "valid durations validate clean",
			gaggleControls:   "  runControls:\n    stalledRunTimeout: 45m\n",
			workflowControls: "  runControls:\n    maxRunDuration: 2h\n",
		},
		{
			name:           "gaggle stalledRunTimeout invalid",
			gaggleControls: "  runControls:\n    stalledRunTimeout: sweepprobe\n",
			wantErr:        true,
			want:           `Gaggle/acme: spec.runControls.stalledRunTimeout "sweepprobe" is not a valid duration; use Go duration syntax, e.g. "45m" or "2h"`,
		},
		{
			name:             "workflow maxRunDuration invalid",
			workflowControls: "  runControls:\n    maxRunDuration: sweepprobe\n",
			wantErr:          true,
			want:             `Workflow/sweep: spec.runControls.maxRunDuration "sweepprobe" is not a valid duration; use Go duration syntax, e.g. "45m" or "2h"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config := runControlsConfig(tc.gaggleControls, tc.workflowControls)
			if err := os.WriteFile(filepath.Join(dir, "budgets.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			got := joinIssues(report)
			if !tc.wantErr {
				if strings.Contains(got, "runControls") {
					t.Fatalf("expected no runControls diagnostic, got:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("diagnostics missing %q:\n%s", tc.want, got)
			}
			if strings.Contains(got, "time: invalid duration") {
				t.Errorf("diagnostics leak the raw Go parse error:\n%s", got)
			}
		})
	}
}
