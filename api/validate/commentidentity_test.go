package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commentIdentityWorkflow is a single-stage workflow running command with the
// given identityMode ("" leaves it unset).
func commentIdentityWorkflow(name, command, identityMode string, actions, capabilities []string) string {
	inputs := "      inputs:\n        resultFile: \"result.json\"\n"
	if identityMode != "" {
		inputs += "        identityMode: \"" + identityMode + "\"\n"
	}
	caps, acts := "", ""
	for _, a := range actions {
		acts += "        - " + a + "\n"
	}
	for _, c := range capabilities {
		caps += "        - " + c + "\n"
	}
	return "apiVersion: goobers.dev/v1alpha1\nkind: Workflow\ndslVersion: \"2.0\"\nmetadata:\n  name: " + name + "\n" +
		"spec:\n  gaggle: acme-web\n  triggers:\n    - type: schedule\n      schedule: \"*/10 * * * *\"\n" +
		"  start: " + name + "\n  tasks:\n    - name: " + name + "\n      type: deterministic\n" +
		"      goal: Exercise the comment classifier settings.\n" +
		"      run:\n        command: [\"goobers\", \"" + command + "\"]\n" + inputs +
		"      capabilities:\n" + caps + "      policyActions:\n" + acts
}

// TestCommentIdentityDisagreementWarns (#6494): pr-comment-watch and a healer
// resolving identityMode differently in one gaggle draws WF029; agreeing
// settings (explicit or defaulted alike) do not.
func TestCommentIdentityDisagreementWarns(t *testing.T) {
	tests := []struct {
		name         string
		watchMode    string
		healerMode   string
		wantWarnings int
	}{
		{name: "shared watcher, defaulted dedicated healer on GitHub", watchMode: "shared", wantWarnings: 1},
		{name: "both defaulted", wantWarnings: 0},
		{name: "both shared", watchMode: "shared", healerMode: "shared", wantWarnings: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.CopyFS(root, os.DirFS("../../config-examples")); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "gaggles", "acme-web", "workflows")
			files := map[string]string{
				"comment-watch.yaml": commentIdentityWorkflow("comment-watch", "pr-comment-watch", tt.watchMode, []string{"label-issue"}, []string{"github:issues:write"}),
				"behind-update.yaml": commentIdentityWorkflow("behind-update", "update-behind-pr", tt.healerMode, []string{"clear-remediation", "update-pr-branch"}, []string{"github:pr:write", "github:issues:write"}),
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			report, err := newV(t).ValidateDir(root)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			if report.HasErrors() {
				t.Fatalf("fixture must validate:\n%s", joinIssues(report))
			}
			var got []CodedWarning
			for _, warning := range report.Warnings() {
				if warning.Code == WarningCommentIdentityDisagreement {
					got = append(got, warning)
				}
			}
			if len(got) != tt.wantWarnings {
				t.Fatalf("WF029 warnings = %+v, want %d", got, tt.wantWarnings)
			}
			if tt.wantWarnings > 0 && !strings.Contains(got[0].Explanation, "identityMode=shared") {
				t.Fatalf("WF029 explanation = %q, want each stage's resolved setting named", got[0].Explanation)
			}
		})
	}
}
