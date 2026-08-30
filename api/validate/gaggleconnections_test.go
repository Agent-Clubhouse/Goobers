package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGaggleConnectionRefDiagnostics covers MGV-4's repo-token-ref coherence
// (#1011): a gaggle's connectionRef (project, backlog, or an additionalRepos
// entry) must name a Connection declared in the Manifest. A dangling reference
// is a half-configured foreign gaggle — it must fail closed at `validate` time
// with a message naming the field and the missing connection, not surface as an
// opaque credential-resolution failure at runtime. An empty connectionRef is
// left alone: at local tiers a gaggle binds its repo token per-repo in
// instance.yaml rather than through a Manifest Connection.
func TestGaggleConnectionRefDiagnostics(t *testing.T) {
	tests := []struct {
		name            string
		projectConnRef  string
		backlogConnRef  string
		additionalRepo  string // full additionalRepos block, or "" to omit
		declareGithub   bool   // declare the "github-main"/"github-backlog" connections
		want            []string
		wantNoConnError bool
	}{
		{
			name:            "valid — refs resolve to declared connections",
			projectConnRef:  "    connectionRef: github-main",
			backlogConnRef:  "    connectionRef: github-backlog",
			declareGithub:   true,
			wantNoConnError: true,
		},
		{
			name:            "no connectionRef anywhere is allowed (local-tier binding)",
			projectConnRef:  "",
			backlogConnRef:  "",
			declareGithub:   false,
			wantNoConnError: true,
		},
		{
			name:           "dangling project connectionRef",
			projectConnRef: "    connectionRef: ghost-conn",
			backlogConnRef: "",
			declareGithub:  false,
			want: []string{
				`Gaggle/acme: spec.project.connectionRef names connection "ghost-conn", but no Connection/ghost-conn is declared in the Manifest`,
			},
		},
		{
			name:           "dangling backlog connectionRef",
			projectConnRef: "",
			backlogConnRef: "    connectionRef: ghost-backlog",
			declareGithub:  false,
			want: []string{
				`Gaggle/acme: spec.backlog.connectionRef names connection "ghost-backlog", but no Connection/ghost-backlog is declared in the Manifest`,
			},
		},
		{
			name:           "dangling additionalRepos connectionRef",
			projectConnRef: "    connectionRef: github-main",
			backlogConnRef: "    connectionRef: github-backlog",
			declareGithub:  true,
			additionalRepo: `  additionalRepos:
    - provider: github
      owner: acme
      name: docs
      connectionRef: ghost-repo`,
			want: []string{
				`Gaggle/acme: spec.additionalRepos[0].connectionRef names connection "ghost-repo", but no Connection/ghost-repo is declared in the Manifest`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			connections := ""
			if tc.declareGithub {
				connections = `  connections:
    - name: github-main
      type: repo
      provider: github
      secretRef:
        name: github-pat
    - name: github-backlog
      type: backlog
      provider: github
      secretRef:
        name: github-pat`
			}
			config := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: foreign
spec:
  instance:
    name: foreign
    environment: dev
%s
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
%s
%s
  isolation:
    namespace: gaggle-acme
`, connections, tc.projectConnRef, tc.backlogConnRef, tc.additionalRepo)

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "foreign.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}

			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			got := joinIssues(report)

			if tc.wantNoConnError {
				// REF012's own warning (declared-but-unhonored connections) is
				// covered separately; here only a REF004 resolution ERROR is
				// out of place.
				for _, issue := range report.Issues {
					if issue.Severity == Error && strings.Contains(issue.Message, "connectionRef") {
						t.Fatalf("expected no connectionRef error, got:\n%s", got)
					}
				}
				return
			}
			if !report.HasErrors() {
				t.Fatalf("dangling connectionRef reported no errors")
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostics missing %q:\n%s", want, got)
				}
			}
			// A dangling ref is reported exactly once, at its own gaggle.
			for _, want := range tc.want {
				if n := strings.Count(got, want); n != 1 {
					t.Errorf("expected diagnostic %q exactly once, saw %d:\n%s", want, n, got)
				}
			}
		})
	}
}

// TestGaggleConnectionRefUnhonoredWarning covers REF012 (#3296): connectionRef
// reads as a credential selector but the runtime never consults it — every
// credentialed capability a gaggle's stages hold comes from that gaggle's own
// repo binding, selected from instance.yaml by repository identity. A gaggle
// that names two DIFFERENT connections is therefore asking for two credentials
// and gets one, so the losing declaration must be surfaced instead of silently
// substituted. A gaggle whose sites all name ONE connection has nothing to
// substitute and stays quiet.
func TestGaggleConnectionRefUnhonoredWarning(t *testing.T) {
	tests := []struct {
		name           string
		projectConnRef string
		backlogConnRef string
		additionalRepo string
		want           []string
	}{
		{
			name:           "one connection everywhere is honored",
			projectConnRef: "    connectionRef: github-main",
			backlogConnRef: "    connectionRef: github-main",
		},
		{
			name:           "no connectionRef anywhere",
			projectConnRef: "",
			backlogConnRef: "",
		},
		{
			name:           "backlog names a second connection",
			projectConnRef: "    connectionRef: github-main",
			backlogConnRef: "    connectionRef: github-backlog",
			want: []string{
				`spec.backlog.connectionRef names connection "github-backlog", but this gaggle already selects connection "github-main"`,
			},
		},
		{
			name:           "reference repo names a second connection, project declares none",
			projectConnRef: "",
			backlogConnRef: "    connectionRef: github-backlog",
			additionalRepo: `  additionalRepos:
    - provider: github
      owner: acme
      name: docs
      connectionRef: github-main`,
			want: []string{
				`spec.additionalRepos[0].connectionRef names connection "github-main", but this gaggle already selects connection "github-backlog"`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: foreign
spec:
  instance:
    name: foreign
    environment: dev
  connections:
    - name: github-main
      type: repo
      provider: github
      secretRef:
        name: github-pat
    - name: github-backlog
      type: backlog
      provider: github
      secretRef:
        name: github-pat
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
%s
%s
  isolation:
    namespace: gaggle-acme
`, tc.projectConnRef, tc.backlogConnRef, tc.additionalRepo)

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "foreign.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			got := joinIssues(report)
			if report.HasErrors() {
				t.Fatalf("declared connections resolve, so REF012 must never be an error:\n%s", got)
			}

			var found []Issue
			for _, issue := range report.Issues {
				if issue.Code == WarningConnectionRefUnhonored {
					found = append(found, issue)
				}
			}
			if len(found) != len(tc.want) {
				t.Fatalf("REF012 findings = %d, want %d:\n%s", len(found), len(tc.want), got)
			}
			for _, issue := range found {
				if issue.Severity != Warning {
					t.Errorf("REF012 severity = %v, want Warning", issue.Severity)
				}
			}
			for _, want := range tc.want {
				if n := strings.Count(got, want); n != 1 {
					t.Errorf("expected %q exactly once, saw %d:\n%s", want, n, got)
				}
			}
		})
	}
}
