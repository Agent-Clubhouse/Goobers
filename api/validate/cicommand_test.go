package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGaggleCICommandDiagnostics covers MGV-4's CI-command coherence (#1011)
// over the per-gaggle ciCommand surface (#1009). The gaggle schema already
// rejects an empty command and empty-string elements; this exercises the one
// exec-fatal shape the schema cannot express — a program (argv[0]) carrying
// whitespace, most often the whole command written as a single string. A
// well-formed split command validates clean.
func TestGaggleCICommandDiagnostics(t *testing.T) {
	tests := []struct {
		name    string
		ci      string // YAML value for ciCommand, or "" to omit the field
		want    string // required substring when wantErr
		wantErr bool
	}{
		{name: "well-formed split command validates clean", ci: `["npm", "run", "ci"]`},
		{name: "no ciCommand is unchanged (single Go gaggle)", ci: ``},
		{
			name:    "whole command as one string",
			ci:      `["npm run ci"]`,
			wantErr: true,
			want:    `Gaggle/acme: spec.ciCommand program "npm run ci" contains whitespace`,
		},
		{
			name:    "blank program",
			ci:      `[" "]`,
			wantErr: true,
			want:    `Gaggle/acme: spec.ciCommand program " " contains whitespace`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ciLine := ""
			if tc.ci != "" {
				ciLine = "  ciCommand: " + tc.ci
			}
			config := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: foreign
spec:
  instance:
    name: foreign
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
    owner: acme
    name: app
%s
  backlog:
    provider: github
    project: acme/app
  isolation:
    namespace: gaggle-acme
`, ciLine)

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "foreign.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			got := joinIssues(report)
			if !tc.wantErr {
				if strings.Contains(got, "ciCommand") {
					t.Fatalf("expected no ciCommand diagnostic, got:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("diagnostics missing %q:\n%s", tc.want, got)
			}
		})
	}
}
