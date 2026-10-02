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

// A GitHub project with an ADO backlog stays refused: nothing in the gaggle
// names the backlog's ADO organization, so its stages would address the
// wrong service.
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
	if !strings.Contains(issue.Message, "only for Azure DevOps code") {
		t.Fatalf("diagnostic must name the supported mixed topology, got: %q", issue.Message)
	}
	if issue.Name != "alpha" {
		t.Fatalf("expected the issue to name gaggle alpha, got %q", issue.Name)
	}
}

// Topology (b) (ADO-N31): a GitHub backlog for an ADO project is routed by
// role and validates cleanly — the ADO-N13 guard is lifted for it.
func TestGaggleGitHubBacklogForADOProjectAccepted(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithProviders(t, dir, "gaggle.yaml", "alpha", "ado", "github", "")

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, errorGaggleMixedProviderADO, Error); ok {
		t.Fatalf("a GitHub backlog for ADO code must validate: %v", report.Issues)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, WarningGaggleMixedProvider, Warning); ok {
		t.Fatalf("a GitHub backlog for ADO code must not warn: %v", report.Issues)
	}
}

// A Gitea backlog for an ADO project uses the same routing.
func TestGaggleGiteaBacklogForADOProjectAccepted(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithProviders(t, dir, "gaggle.yaml", "alpha", "ado", "gitea", "https://gitea.example.com")

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if _, ok := issueWithCodeAndSeverity(t, report, errorGaggleMixedProviderADO, Error); ok {
		t.Fatalf("a Gitea backlog for ADO code must validate: %v", report.Issues)
	}
}

// Topology (b) finds the backlog repository, and its credential, by the
// owner/name in spec.backlog.project, so any other shape is refused.
func TestGaggleGitHubBacklogForADOProjectNeedsOwnerName(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithProviders(t, dir, "gaggle.yaml", "alpha", "ado", "github", "")
	path := filepath.Join(dir, "gaggle.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(string(raw), "project: acme/web\n", "project: web\n", 1)
	if doc == string(raw) {
		t.Fatalf("fixture has no backlog owner/name to rewrite:\n%s", raw)
	}
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	issue, ok := issueWithCodeAndSeverity(t, report, errorGaggleMixedProviderADO, Error)
	if !ok {
		t.Fatalf("expected %s error for a backlog project that is not owner/name, got: %v", errorGaggleMixedProviderADO, report.Issues)
	}
	if !strings.Contains(issue.Message, "owner/name") {
		t.Fatalf("diagnostic must name the owner/name requirement, got: %q", issue.Message)
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
