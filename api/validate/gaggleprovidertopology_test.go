package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGaggleWithProviders writes a minimal Gaggle whose project and backlog
// live on the given (possibly different) providers. baseURL is only needed
// when a provider is gitea; it is written on both refs when non-empty.
func writeGaggleWithProviders(t *testing.T, dir, file, name, projectProvider, backlogProvider, baseURL string) {
	t.Helper()
	projectExtra := ""
	backlogExtra := ""
	// An ADO backlog names its own ADO project (backlog-project), distinct
	// from the code project (web), so the ado/ado case models the supported
	// ADO project split; other providers use an owner/repo backlog string.
	backlogProject := "acme/web"
	if backlogProvider == "ado" {
		backlogProject = "backlog-project"
	}
	if projectProvider == "ado" {
		projectExtra = "\n    project: code-project"
	}
	if baseURL != "" {
		if projectProvider == "gitea" {
			projectExtra += "\n    baseUrl: " + baseURL
		}
		if backlogProvider == "gitea" {
			backlogExtra += "\n    baseUrl: " + baseURL
		}
	}
	doc := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: ` + name + `
spec:
  project:
    provider: ` + projectProvider + `
    owner: acme
    name: web` + projectExtra + `
  backlog:
    provider: ` + backlogProvider + `
    project: ` + backlogProject + backlogExtra + `
  isolation:
    namespace: gaggle-` + name + `
`
	if err := os.WriteFile(filepath.Join(dir, file), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func issueWithCodeAndSeverity(t *testing.T, report *Report, code WarningCode, sev Severity) (Issue, bool) {
	t.Helper()
	for _, issue := range report.Issues {
		if issue.Code == code && issue.Severity == sev {
			return issue, true
		}
	}
	return Issue{}, false
}

// A GitHub project with an ADO backlog is refused outright (ADO-N13): every
// backlog stage today opens the routed *project* provider, so this topology
// would silently query ADO Boards through the GitHub-routed path (or vice
// versa) instead of failing loudly.
func TestGaggleMixedProvidersRejected(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithProviders(t, dir, "gaggle.yaml", "alpha", "github", "ado", "")

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	issue, ok := issueWithCodeAndSeverity(t, report, errorGaggleMixedProviderADO, Error)
	if !ok {
		t.Fatalf("expected %s error, got: %v", errorGaggleMixedProviderADO, report.Issues)
	}
	if !strings.Contains(issue.Message, "v0.5.x") || !strings.Contains(issue.Message, "ADO-N31") {
		t.Fatalf("diagnostic must name v0.5.x/ADO-N31 as the release adding support, got: %q", issue.Message)
	}
	if issue.Name != "alpha" {
		t.Fatalf("expected the issue to name gaggle alpha, got %q", issue.Name)
	}
}

// The reverse split (ADO project, GitHub backlog) is refused the same way.
func TestGaggleMixedProvidersADOProjectGitHubBacklogRejected(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithProviders(t, dir, "gaggle.yaml", "alpha", "ado", "github", "")

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, errorGaggleMixedProviderADO, Error); !ok {
		t.Fatalf("expected %s error, got: %v", errorGaggleMixedProviderADO, report.Issues)
	}
}

// A mismatch between two non-ADO providers (GitHub project, Gitea backlog)
// is warned rather than refused, so no existing non-ADO config breaks under
// the no-breaking-change goal.
func TestGaggleMixedNonADOProvidersWarnsOnly(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithProviders(t, dir, "gaggle.yaml", "alpha", "github", "gitea", "https://gitea.example.com")

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, errorGaggleMixedProviderADO, Error); ok {
		t.Fatalf("a non-ADO mismatch must not be a hard error: %v", report.Issues)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, WarningGaggleMixedProvider, Warning); !ok {
		t.Fatalf("expected %s warning, got: %v", WarningGaggleMixedProvider, report.Issues)
	}
}

// An ADO project with the backlog in a *different* ADO project is the
// supported project split: the code repository lives in ADO project
// code-project while spec.backlog.project names backlog-project. Both refs
// share the ado provider, so this must keep passing.
func TestGaggleADOProjectSplitAccepted(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithProviders(t, dir, "gaggle.yaml", "alpha", "ado", "ado", "")

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, errorGaggleMixedProviderADO, Error); ok {
		t.Fatalf("an ADO project split must still pass: %v", report.Issues)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, WarningGaggleMixedProvider, Warning); ok {
		t.Fatalf("an ADO project split must not warn either: %v", report.Issues)
	}
}

// Same-provider GitHub project and backlog (the common case) is unaffected.
func TestGaggleSameProviderNoTopologyIssue(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithProviders(t, dir, "gaggle.yaml", "alpha", "github", "github", "")

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, errorGaggleMixedProviderADO, Error); ok {
		t.Fatalf("same-provider project/backlog must not error: %v", report.Issues)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, WarningGaggleMixedProvider, Warning); ok {
		t.Fatalf("same-provider project/backlog must not warn: %v", report.Issues)
	}
}
