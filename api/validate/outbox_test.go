package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// outboxConfig builds a single-gaggle Manifest+Gaggle+Workflow where the
// gaggle-level mirror block, the workflow-level mirror block, and the task's
// outbox block are substituted verbatim ("" omits each) — the minimal shape
// exercising every OUT001 surface (#3662).
func outboxConfig(gaggleMirror, workflowMirror, taskOutbox string) string {
	return fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: exports
spec:
  instance:
    name: exports
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
dslVersion: "2.0"
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
%s`, gaggleMirror, workflowMirror, taskOutbox)
}

func TestOutboxContainmentAndMirrorRootDiagnostics(t *testing.T) {
	tests := []struct {
		name           string
		gaggleMirror   string
		workflowMirror string
		taskOutbox     string
		want           string
		wantErr        bool
	}{
		{
			name:           "valid declarations validate clean",
			gaggleMirror:   "  outboxMirrorPath: ~/goobers/outbox\n",
			workflowMirror: "  outboxMirrorPath: /var/lib/goobers/outbox\n",
			taskOutbox:     "      outbox: [\"report.md\", \"reports/summary.json\"]\n",
		},
		{
			name:         "relative gaggle mirror root",
			gaggleMirror: "  outboxMirrorPath: reports\n",
			wantErr:      true,
			want:         `Gaggle/acme: spec.outboxMirrorPath: invalid outbox mirror root: "reports" must be absolute or start with ~/`,
		},
		{
			name:           "relative workflow mirror root",
			workflowMirror: "  outboxMirrorPath: ./reports\n",
			wantErr:        true,
			want:           `Workflow/sweep: spec.outboxMirrorPath: invalid outbox mirror root: "./reports" must be absolute or start with ~/`,
		},
		{
			name:       "escaping task outbox entry",
			taskOutbox: "      outbox: [\"../outside\"]\n",
			wantErr:    true,
			want:       `Workflow/sweep: task "noop" outbox[0]`,
		},
		{
			name:       "absolute task outbox entry",
			taskOutbox: "      outbox: [\"/etc/passwd\"]\n",
			wantErr:    true,
			want:       `Workflow/sweep: task "noop" outbox[0]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config := outboxConfig(tc.gaggleMirror, tc.workflowMirror, tc.taskOutbox)
			if err := os.WriteFile(filepath.Join(dir, "exports.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			got := joinIssues(report)
			if !tc.wantErr {
				if strings.Contains(got, "OUT001") {
					t.Fatalf("expected no outbox diagnostic, got:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, "OUT001") {
				t.Errorf("diagnostics missing OUT001:\n%s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("diagnostics missing %q:\n%s", tc.want, got)
			}
		})
	}
}

// TestEmptyOutboxEntryIsRejectedBySchema pins the schema half: an empty string
// in the outbox array is refused at document validation, before the semantic
// pass ever sees it.
func TestEmptyOutboxEntryIsRejectedBySchema(t *testing.T) {
	dir := t.TempDir()
	config := outboxConfig("", "", "      outbox: [\"\"]\n")
	if err := os.WriteFile(filepath.Join(dir, "exports.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	got := joinIssues(report)
	if !strings.Contains(got, "outbox") {
		t.Fatalf("expected an outbox diagnostic for an empty entry, got:\n%s", got)
	}
}
