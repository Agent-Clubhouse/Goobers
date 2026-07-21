package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGaggleBranchNamespaceDiagnostics covers MGV-4's branch-prefix coherence
// (#1011) over the per-gaggle branchNamespace surface (#965/#1010). The gaggle
// schema pattern already enforces the ref-path structure; this exercises the
// gap it cannot express — a structurally valid value that would nonetheless
// produce an INVALID git branch name at runtime (branchNamespace becomes a live
// run branch "<namespace><workflow>/<run>"). The invalid set matches git's own
// check-ref-format rules exactly: a component ending in ".lock", or ".." in the
// value. A trailing-dot component ("team.") is accepted mid-ref by git, so it is
// deliberately NOT flagged; well-formed namespaces validate clean.
func TestGaggleBranchNamespaceDiagnostics(t *testing.T) {
	tests := []struct {
		name    string
		ns      string // YAML value for branchNamespace, or "" to omit
		wantErr bool
	}{
		{name: "conventional namespace validates clean", ns: "team-web/"},
		{name: "no trailing slash validates clean", ns: "team-web"},
		{name: "nested namespace validates clean", ns: "team/sub/"},
		{name: "trailing-dot component ok (git accepts mid-ref)", ns: "team./"},
		{name: "omitted uses the default", ns: ""},
		{name: "component ending in .lock", ns: "acme.lock/", wantErr: true},
		{name: "consecutive dots", ns: "a..b/", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nsLine := ""
			if tc.ns != "" {
				nsLine = "  branchNamespace: " + tc.ns
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
`, nsLine)

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "foreign.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			got := joinIssues(report)
			hasErr := strings.Contains(got, "spec.branchNamespace") &&
				strings.Contains(got, "invalid git run-branch name")
			if tc.wantErr && !hasErr {
				t.Errorf("expected a branchNamespace diagnostic, got:\n%s", got)
			}
			if !tc.wantErr && hasErr {
				t.Errorf("unexpected branchNamespace diagnostic:\n%s", got)
			}
		})
	}
}
