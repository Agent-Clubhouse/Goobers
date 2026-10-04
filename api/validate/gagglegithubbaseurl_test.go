package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub Enterprise Server is unsupported (#6347): a baseUrl on any github
// reference in a gaggle is refused with a diagnostic naming the field and the
// reason, rather than being silently ignored by clone URLs and auth matchers.
func TestGaggleGitHubBaseURLRejected(t *testing.T) {
	dir := t.TempDir()
	doc := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: alpha
spec:
  project:
    provider: github
    baseUrl: https://ghe.example.com
    owner: acme
    name: web
  backlog:
    provider: github
    baseUrl: https://ghe.example.com
    project: acme/web
  additionalRepos:
    - provider: github
      baseUrl: https://ghe.example.com
      owner: acme
      name: docs
  siblings:
    - project:
        provider: github
        baseUrl: https://ghe.example.com
        owner: acme
        name: web
  isolation:
    namespace: gaggle-alpha
`
	if err := os.WriteFile(filepath.Join(dir, "gaggle.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	var got []string
	for _, issue := range report.Issues {
		if issue.Code == errorGaggleGitHubBaseURL && issue.Severity == Error {
			got = append(got, issue.Message)
		}
	}
	want := []string{"spec.project.baseUrl", "spec.backlog.baseUrl", "spec.additionalRepos[0].baseUrl", "spec.siblings[0].project.baseUrl"}
	if len(got) != len(want) {
		t.Fatalf("expected %d %s errors, got %d: %v", len(want), errorGaggleGitHubBaseURL, len(got), report.Issues)
	}
	for _, field := range want {
		found := false
		for _, msg := range got {
			if strings.HasPrefix(msg, field+": ") && strings.Contains(msg, "GitHub Enterprise Server is unsupported") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no diagnostic names %s with the GHES reason; got: %q", field, got)
		}
	}
	if _, ok := issueWithCodeAndSeverity(t, report, errorSchemaViolation, Error); !ok {
		t.Fatalf("the gaggle schema must also refuse a github baseUrl: %v", report.Issues)
	}
}

// A gitea baseUrl stays required and accepted; the github refusal must not
// leak onto other providers.
func TestGaggleGiteaBaseURLStillAccepted(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithProviders(t, dir, "gaggle.yaml", "alpha", "gitea", "gitea", "https://gitea.example.com")

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	for _, issue := range report.Issues {
		if issue.Code == errorGaggleGitHubBaseURL || issue.Code == errorSchemaViolation {
			t.Fatalf("a gitea baseUrl must validate: %v", report.Issues)
		}
	}
}
